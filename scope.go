package loom

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// turnState is the state shared across one Turn execution.
//
// The turn root and every step share one pointer to it, which guarantees that:
//   - closeReason is globally visible: once any source sets it, every scope refuses to
//     write
//   - the items tree has a single authority, with each scope pointing at its own subtree
//   - sink calls are serialised through the shared mu
//
// Every access to a mutable field must hold mu.
type turnState struct {
	mu sync.Mutex

	// ===== Immutable fields, set at construction =====
	turnIdx        uint64
	turnPath       string // "turn[0]"
	conversationID string // first-class; Run checks it is non-empty

	sinks      []Sink
	onSinkErr  func(Sink, error)
	strictSink bool
	metadata   map[string]string

	// captureContent controls whether OTel spans record the prompt, the completion, tool
	// args, and tool output. False keeps only metadata — model, tokens, finish reason,
	// latency — which suits production data with PII or compliance sensitivity. It comes
	// from RunOptions.CaptureContent.
	captureContent bool

	// ===== Mutable fields, guarded by mu =====

	// closeReason: nil means still running, non-nil means sealed. Once set, Status derives
	// from CloseReason.Code as Completed, Cancelled, or Failed. The sources that set it:
	//   - a successful FinalAnswer or StreamFinalAnswer → {Completed, "final_answer"}
	//   - an external cancel, ctx.Cancel                → {Cancelled, "user_cancel"}
	//   - a timeout, ctx.DeadlineExceeded               → {Cancelled, "timeout"}
	//   - the handler returning an error                → {Failed, "agent_error" / "content_filter" / ...}
	//   - a strict-mode sink failure                    → {Failed, "agent_error"} with Cause=sinkErr
	closeReason *CloseReason

	// items is the root of Turn.Items; the nesting goes through Item.Children.
	items []Item

	// sinkErr records the first sink failure in strict mode, which Run checks at the end to
	// fail the Turn.
	sinkErr error

	// callIDCounter is the 0-based counter loom uses when it generates a tool call ID,
	// incrementing within the turn. ExecuteToolCalls, the LLM-driven case, uses the
	// ToolCall.ID the model supplied and leaves this alone; RunToolByName, the
	// code-orchestration case, builds "call_0", "call_1", and so on from it.
	callIDCounter uint64

	// totalUsage accumulates token usage over every LLM call in this Turn. emitLLMCalled
	// adds to it under the lock, alongside each ancestor step's Usage, so it is the turn
	// root's counterpart. buildTurnSnapshot copies it to Turn.Usage at the end of Run.
	totalUsage Usage

	// createdAt is when the Turn started, used when Run builds the snapshot.
	createdAt time.Time
}

// newTurnState builds a turnState for Run. It is unexported.
func newTurnState(
	turnIdx uint64,
	conversationID string,
	sinks []Sink,
	onSinkErr func(Sink, error),
	strictSink bool,
	metadata map[string]string,
	captureContent bool,
) *turnState {
	return &turnState{
		turnIdx:        turnIdx,
		turnPath:       formatTurnPath(turnIdx),
		conversationID: conversationID,
		sinks:          sinks,
		onSinkErr:      onSinkErr,
		strictSink:     strictSink,
		metadata:       metadata,
		captureContent: captureContent,
		createdAt:      time.Now(),
	}
}

// isClosed is called with mu already held. True means the Turn is sealed, so any write
// should return ErrTurnClosed.
func (s *turnState) isClosed() bool {
	return s.closeReason != nil
}

// nextToolCallIDLocked is called with mu already held, and returns an incrementing
// "call_N" ID. Only helpers such as RunToolByName use it, in the code-orchestration case;
// an LLM-driven call uses its ToolCall.ID directly.
func (s *turnState) nextToolCallIDLocked() string {
	id := "call_" + strconv.FormatUint(s.callIDCounter, 10)
	s.callIDCounter++
	return id
}

// formatTurnPath turns a turn index into "turn[N]".
func formatTurnPath(idx uint64) string {
	return "turn[" + strconv.FormatUint(idx, 10) + "]"
}

// writerScope is the scope a Writer or Step lives in. The turn root and every step use
// the same type; they differ in the index chain, the path from the turn items root to
// this scope.
//
// Every method, writing or nesting, is serialised through state.mu, so a scope carries no
// lock of its own.
//
// Concurrency: one scope may not be used from several goroutines, and a parent and child
// scope may not be used at the same time, so a child step closure must not touch the
// outer scope. Breaking this is undefined behaviour.
type writerScope struct {
	state *turnState // the Turn's shared state

	// path is the full path, such as "turn[0].step[0].step[1]". At the turn root it equals
	// state.turnPath.
	path string

	// indices locates this scope from the state items root:
	//   - root scope:nil(children = &state.items)
	//   - a child scope: [a, b, c] → children = &state.items[a].Children[b].Children[c].Children
	//
	// Every write re-walks the indices through locateChildrenLocked, so a slice that grows
	// does not leave a cached address dangling.
	indices []int

	// counters holds a 0-based counter per ItemKind in this scope. Writing a new item takes
	// the current value as its Item.Index and then increments it.
	counters map[ItemKind]uint64
}

// newRootScope builds the Turn root scope, which turnRoot holds.
func newRootScope(state *turnState) *writerScope {
	return &writerScope{
		state:    state,
		path:     state.turnPath,
		counters: map[ItemKind]uint64{},
		indices:  nil,
	}
}

// locateChildrenLocked walks the index chain to this scope's children slice. state.mu must
// already be held. It re-walks every time, so the address stays correct after a slice
// grows.
func (w *writerScope) locateChildrenLocked() *[]Item {
	if len(w.indices) == 0 {
		return &w.state.items
	}
	container := &w.state.items
	for _, idx := range w.indices[:len(w.indices)-1] {
		container = &(*container)[idx].Children
	}
	last := w.indices[len(w.indices)-1]
	return &(*container)[last].Children
}

// Path returns this scope's full path, such as "turn[0].step[0]".
func (w *writerScope) Path() string {
	return w.path
}

// underlyingScope implements scopeAccessor for in-package helpers such as RunToolByName,
// which need the turnState. step and turnRoot embed *writerScope and inherit this
// method.
func (w *writerScope) underlyingScope() *writerScope {
	return w
}

// ===== Sink fan-out helpers =====
// These methods hold no lock: the caller invokes them after releasing mu, so a slow Sink
// cannot block other writes in the same Turn.

// emitItemStarted broadcasts ItemStartedEvent to every sink.
func (s *turnState) emitItemStarted(ctx context.Context, item Item) {
	ev := ItemStartedEvent{
		TurnIndex: s.turnIdx,
		TurnPath:  s.turnPath,
		Item:      item,
		Time:      time.Now(),
	}
	s.fanOut(func(sink Sink) error { return sink.ItemStarted(ctx, ev) })
}

// emitItemDelta broadcasts ItemDeltaEvent to every sink.
func (s *turnState) emitItemDelta(ctx context.Context, itemPath string, ch DeltaChannel, chunk string) {
	ev := ItemDeltaEvent{
		TurnIndex: s.turnIdx,
		TurnPath:  s.turnPath,
		ItemPath:  itemPath,
		Channel:   ch,
		Chunk:     chunk,
		Time:      time.Now(),
	}
	s.fanOut(func(sink Sink) error { return sink.ItemDelta(ctx, ev) })
}

// emitItemFinished broadcasts ItemFinishedEvent to every sink.
func (s *turnState) emitItemFinished(ctx context.Context, item Item) {
	ev := ItemFinishedEvent{
		TurnIndex: s.turnIdx,
		TurnPath:  s.turnPath,
		Item:      item,
		Time:      time.Now(),
	}
	finishCtx := context.WithoutCancel(ctx)
	s.fanOut(func(sink Sink) error { return sink.ItemFinished(finishCtx, ev) })
}

// emitLLMCalled accumulates one LLM call's usage up the index chain into every ancestor
// step's Item.Usage, then fans LLMCalledEvent out to every sink.
//
// It accumulates from indices[0] down the items tree, adding to every ancestor whose Kind
// is step. A call hanging off the turn root, with nil indices, also comes through here,
// where it only fans out and accumulates into no step.
//
// stepPath is the scope path, the same as turnPath at the turn root, and becomes an empty
// string when fanned out to a sink. model and purpose pass through for observability.
//
// Serialisation: as with emitItem*, the fan-out happens outside the lock, so
// Sink.LLMCalled must not assume mu is held.
func (s *turnState) emitLLMCalled(ctx context.Context, scope *writerScope, model, purpose string, usage Usage) {
	s.mu.Lock()
	if !s.isClosed() {
		// Accumulate item.Usage up the index chain, skipping the turn root where indices is
		// empty
		container := &s.items
		for _, idx := range scope.indices {
			addUsage(&(*container)[idx].Usage, usage)
			container = &(*container)[idx].Children
		}
		// Accumulate at the turn root
		addUsage(&s.totalUsage, usage)
	}
	s.mu.Unlock()

	stepPath := scope.path
	if stepPath == s.turnPath {
		stepPath = "" // the turn root
	}
	ev := LLMCalledEvent{
		TurnIndex: s.turnIdx,
		TurnPath:  s.turnPath,
		StepPath:  stepPath,
		Model:     model,
		Purpose:   purpose,
		Usage:     usage,
		Time:      time.Now(),
	}
	s.fanOut(func(sink Sink) error { return sink.LLMCalled(ctx, ev) })
}

// addUsage adds b into a, field by field.
func addUsage(a *Usage, b Usage) {
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.CachedTokens += b.CachedTokens
	a.ReasoningTokens += b.ReasoningTokens
	a.TotalTokens += b.TotalTokens
}

// fanOut calls every sink serially and synchronously. On a failure it calls onSinkErr when
// set, and in strict mode records the first failure in state.sinkErr, which Run uses at
// the end to fail the Turn.
func (s *turnState) fanOut(fn func(Sink) error) {
	for _, sink := range s.sinks {
		if err := fn(sink); err != nil {
			if s.onSinkErr != nil {
				s.onSinkErr(sink, err)
			}
			if s.strictSink {
				s.mu.Lock()
				if s.sinkErr == nil {
					s.sinkErr = err
				}
				s.mu.Unlock()
			}
		}
	}
}

// ===== One-shot write methods =====

// WriteReasoning writes one block of model reasoning, Completed at once.
func (w *writerScope) WriteReasoning(ctx context.Context, label, text string) error {
	return w.writeSimpleItem(ctx, ItemKindReasoning, func(it *Item) {
		it.Label = label
		it.Text = text
	})
}

// WriteToolCall writes a record of one tool call, Completed at once. It does not run the
// tool: product code runs it and pairs the outcome with WriteToolResult.
func (w *writerScope) WriteToolCall(ctx context.Context, label string, call ToolCall) error {
	return w.writeSimpleItem(ctx, ItemKindToolCall, func(it *Item) {
		it.Label = label
		it.ToolName = call.Name
		it.ToolCallID = call.ID
		it.Arguments = call.Arguments
	})
}

// WriteToolResult writes the result of running one tool. result.CallID must pair with
// ToolCall.CallID from the earlier WriteToolCall, and result.ToolName must match that
// call's Name, which keeps the tool_call and tool_result rows symmetrical and lets history
// be queried by tool, as a citation loader scanning web_search output does. A non-nil
// result.Err sets Status=Failed; otherwise it is Completed.
func (w *writerScope) WriteToolResult(ctx context.Context, label string, result ToolResult) error {
	return w.writeSimpleItem(ctx, ItemKindToolResult, func(it *Item) {
		it.Label = label
		it.ToolName = result.ToolName
		it.ToolCallID = result.CallID
		it.Output = result.Output
		if result.Err != nil {
			it.Status = ItemStatusFailed
			it.Error = result.Err
		}
	})
}

// ===== Streaming writes (closures) =====

// StreamReasoning streams one block of model reasoning.
//
// Closure semantics: a nil return sets Status=Completed and Text to the SetFinalText value
// or the accumulated one, while a non-nil error sets Status=Failed, fills Error, and keeps
// the accumulated Text as a partial result.
func (w *writerScope) StreamReasoning(ctx context.Context, label string, fn func(ReasoningStream) error) error {
	selfIdx, item, ok := w.openStreamItem(ItemKindReasoning, label, func(it *Item) {
		it.Label = label
	})
	if !ok {
		return ErrTurnClosed
	}
	w.state.emitItemStarted(ctx, item)

	stream := &itemTextStream{state: w.state, itemPath: item.Path}
	fnErr := fn(stream)

	snapshot := w.finalizeStreamItem(selfIdx, stream.core.finalize(), fnErr)
	w.state.emitItemFinished(ctx, snapshot)
	return fnErr
}

// openStreamItem is shared by the opening stage of every streaming write:
//   - check sealed
//   - allocate the index and path
//   - build the Item with Status=InProgress and append it to children
//   - return selfIdx, which finalize uses to locate it, plus the item snapshot
//
// ok=false means the Turn is sealed, and the caller should return ErrTurnClosed.
func (w *writerScope) openStreamItem(kind ItemKind, label string, fill func(*Item)) (int, Item, bool) {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	if w.state.isClosed() {
		return 0, Item{}, false
	}
	idx := w.counters[kind]
	w.counters[kind] = idx + 1
	now := time.Now()
	item := Item{
		Kind:      kind,
		Index:     idx,
		Path:      formatChildPath(w.path, kind, idx),
		Label:     label,
		Status:    ItemStatusInProgress,
		StartedAt: now,
		UpdatedAt: now,
	}
	fill(&item)
	children := w.locateChildrenLocked()
	*children = append(*children, item)
	return len(*children) - 1, item, true
}

// finalizeStreamItem is shared by the closing stage of every streaming write. It decides
// Status from fnErr, updates Text, Status, and Error, and returns the final snapshot.
func (w *writerScope) finalizeStreamItem(selfIdx int, finalText string, fnErr error) Item {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	children := w.locateChildrenLocked()
	final := &(*children)[selfIdx]
	final.Text = finalText
	if fnErr != nil {
		if IsCancelError(fnErr) {
			final.Status = ItemStatusCancelled
			final.Error = nil
		} else {
			final.Status = ItemStatusFailed
			final.Error = &ItemError{Code: "stream_failed", Message: fnErr.Error()}
		}
	} else {
		final.Status = ItemStatusCompleted
	}
	final.UpdatedAt = time.Now()
	return *final
}

// step implements the Step interface. Every Writer method comes from writerScope.
type step struct {
	*writerScope
}

// Compile-time interface assertions.
var (
	_ Writer = (*writerScope)(nil)
	_ Step   = (*step)(nil)
)

// Step opens a nested sub-step under the current scope.
//
// Closure semantics: a nil return closes the step as Completed, while a non-nil error
// closes it as Failed with Error filled in and propagates.
//
// state.mu is not held while the closure runs, so product code can safely do something
// slow such as an LLM call.
func (w *writerScope) Step(ctx context.Context, label string, fn func(context.Context, Step) error) error {
	// 1. lock, check the seal, allocate the step item
	w.state.mu.Lock()
	if w.state.isClosed() {
		w.state.mu.Unlock()
		return ErrTurnClosed
	}
	idx := w.counters[ItemKindStep]
	w.counters[ItemKindStep] = idx + 1
	path := formatChildPath(w.path, ItemKindStep, idx)
	now := time.Now()
	item := Item{
		Kind:      ItemKindStep,
		Index:     idx,
		Path:      path,
		Label:     label,
		Status:    ItemStatusInProgress,
		StartedAt: now,
		UpdatedAt: now,
	}
	children := w.locateChildrenLocked()
	*children = append(*children, item)
	selfIdx := len(*children) - 1

	// The child scope's indices are the parent's plus selfIdx, copied so the underlying
	// array is not shared
	childIndices := make([]int, len(w.indices)+1)
	copy(childIndices, w.indices)
	childIndices[len(w.indices)] = selfIdx

	w.state.mu.Unlock()

	// 2. emit Started
	w.state.emitItemStarted(ctx, item)

	// 3. OTel: start the child step span. The ctx inside the closure carries it, so a
	// nested StreamLLMToStep or runOneTool becomes a child automatically.
	stepCtx, stepSpan := startStepSpan(ctx, path, label)

	// 4. build the child scope and run the closure
	childScope := &writerScope{
		state:    w.state,
		path:     path,
		indices:  childIndices,
		counters: map[ItemKind]uint64{},
	}
	stepCtx = withUsageScope(stepCtx, childScope)
	fnErr := fn(stepCtx, &step{writerScope: childScope})

	// 5. OTel: finalize the span once the closure returns, before the Item status is
	// updated, since ending a span does not block the synchronous path
	finalizeStepSpan(stepSpan, fnErr)

	// 6. finalize: locate the parent's children again for the address, then set Status and
	// Error
	w.state.mu.Lock()
	parentChildren := w.locateChildrenLocked()
	final := &(*parentChildren)[selfIdx]
	if fnErr != nil {
		if IsCancelError(fnErr) {
			final.Status = ItemStatusCancelled
			final.Error = nil
		} else {
			final.Status = ItemStatusFailed
			final.Error = &ItemError{
				Code:    "step_failed",
				Message: fnErr.Error(),
			}
		}
	} else {
		final.Status = ItemStatusCompleted
	}
	final.UpdatedAt = time.Now()
	snapshot := *final
	w.state.mu.Unlock()

	// 5. emit Finished
	w.state.emitItemFinished(ctx, snapshot)

	return fnErr
}

// writeSimpleItem is the shared skeleton for a one-shot write: reasoning, note, tool call,
// or tool result.
//
// The flow is: check the seal, allocate the index and path, build the Item with
// Status=Completed, append it to childrenPtr, release mu, then emit Started and Finished.
//
// fillKind runs with mu held and fills the Kind-specific fields.
func (w *writerScope) writeSimpleItem(ctx context.Context, kind ItemKind, fillKind func(*Item)) error {
	w.state.mu.Lock()
	if w.state.isClosed() {
		w.state.mu.Unlock()
		return ErrTurnClosed
	}

	idx := w.counters[kind]
	w.counters[kind] = idx + 1
	now := time.Now()
	item := Item{
		Kind:      kind,
		Index:     idx,
		Path:      formatChildPath(w.path, kind, idx),
		Status:    ItemStatusCompleted,
		StartedAt: now,
		UpdatedAt: now,
	}
	fillKind(&item)

	children := w.locateChildrenLocked()
	*children = append(*children, item)

	w.state.mu.Unlock()

	// Emit outside the lock. A one-shot Item sends the same payload in both the Started and
	// the Finished frame.
	w.state.emitItemStarted(ctx, item)
	w.state.emitItemFinished(ctx, item)
	return nil
}

// formatChildPath builds a child item's path. A singleton kind, user_message or
// final_answer, carries no index; every other kind carries "[N]".
func formatChildPath(parentPath string, kind ItemKind, idx uint64) string {
	if isSingletonKind(kind) {
		return parentPath + "." + string(kind)
	}
	return parentPath + "." + string(kind) + "[" + strconv.FormatUint(idx, 10) + "]"
}

// isSingletonKind reports whether a turn may hold only one item of this kind. The state
// machine keeps a singleton from being written twice; WriteFinalAnswer and the like check
// internally.
func isSingletonKind(kind ItemKind) bool {
	return kind == ItemKindUserMessage || kind == ItemKindFinalAnswer
}
