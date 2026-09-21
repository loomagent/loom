package loom

import (
	"context"
	"errors"
	"time"
)

// Handler is the core function product code implements.
//
// It writes the process events through a TurnWriter: Step, Reasoning, ToolCall, and
// finally FinalAnswer. Its error return is how it reports failure:
//   - nil after calling FinalAnswer → the Turn is Completed
//   - nil without FinalAnswer       → the Turn Failed with code=no_final_answer
//   - non-nil                       → the Turn Failed with code=agent_error and cause=err
//
// Run watches for a cancelled or expired ctx and turns it into the matching Cancelled or
// timeout terminal state.
type Handler func(
	ctx context.Context,
	w TurnWriter,
	history []Turn,
	input UserMessage,
) error

// RunOptions holds every Run configuration.
type RunOptions struct {
	// Sinks are the event downstreams. The slice may be empty, which suits a debugging
	// run; the Run core fans out to all of them. Pass several directly in one []Sink; no
	// Tee wrapper is needed.
	//
	// A typical combination is an EntSink for persistence, a LogSink, and a WSSink pushing
	// to a frontend. Each Sink handles the events it cares about; see the consume-as-needed
	// semantics of Sink.ItemDelta.
	Sinks []Sink

	// History holds the finished turns in ascending time order, used to build context. The
	// caller loads it; loom does not say where history comes from, whether a database, a
	// cache, or memory.
	History []Turn

	// Input is this round's user input.
	Input UserMessage

	// ConversationID is the conversation this turn belongs to, and is required: an empty
	// string makes Run fail. It is a first-class value:
	//   - it goes to the loom.conversation.id attribute of the OTel turn span, which lets a
	//     backend aggregate the traces of one conversation
	//   - it is written into the Turn snapshot, so a Repository or Sink can take it without
	//     decoding metadata
	//   - product code loading cross-turn state, such as scanning past citation IDs, uses
	//     the same ID
	ConversationID string

	// TurnIndex is the 0-based position within the conversation. The zero value means
	// "derive it" as uint64(len(History)); when product code loads history page by page and
	// History is therefore incomplete, it must pass a non-zero value.
	TurnIndex uint64

	// Metadata is a K/V passthrough for product code; loom does not interpret it.
	// Conventional keys: "conversation_id", "user_id", "chat_mode".
	Metadata map[string]string

	// OnSinkErr is the sink failure callback; nil by default, which swallows a failure
	// silently.
	OnSinkErr func(Sink, error)

	// StrictSink makes any sink failure fail the whole Turn at once. It is false by default,
	// where a sink failure does not disturb the main flow.
	StrictSink bool

	// CaptureContent controls whether OTel spans record the prompt, the completion, tool
	// args, and tool output. False, the default, keeps only metadata — model, tokens,
	// finish reason, latency — which suits production data with PII or compliance
	// sensitivity. Turn it on while developing, where it makes debugging easier.
	CaptureContent bool
}

// Run executes one agent.
//
// The flow:
//  1. build the turnState and the turnRoot, then register the sinks
//  2. call handler(ctx, root, History, Input)
//  3. derive the final CloseReason from the handler's return value, state.closeReason,
//     the ctx state, and state.sinkErr
//  4. return the *Turn snapshot and an error
//
// The error is set only when the handler returned one itself and a cancelled ctx did not
// cause it. A normal end, a cancellation, and a no_final_answer all return a nil error;
// Turn.CloseReason has the detail.
//
// Note that Run does not write the user_message item. The caller persists it before Run,
// as wolosink.InsertUserMessageItem does, and passes its text in through opts.Input for
// the handler to use. The returned *Turn.Items therefore holds no user_message; load the
// complete item tree from the persistence layer.
func Run(ctx context.Context, h Handler, opts RunOptions) (*Turn, error) {
	if h == nil {
		return validationFailedTurn(opts, "handler must not be empty"), errors.New("loom.Run: handler must not be empty")
	}
	if opts.ConversationID == "" {
		return validationFailedTurn(opts, "ConversationID is required"), errors.New("loom.Run: ConversationID is required")
	}

	turnIdx := opts.TurnIndex
	if turnIdx == 0 {
		turnIdx = uint64(len(opts.History))
	}
	state := newTurnState(turnIdx, opts.ConversationID, opts.Sinks, opts.OnSinkErr, opts.StrictSink, opts.Metadata, opts.CaptureContent)
	root := newTurnRoot(state)

	// OTel: start the Turn root span. The ctx the handler receives carries the span, so
	// child spans started by a Step, by StreamLLMToStep, or inside runOneTool nest under
	// it. Without OTel configured it is the noop tracer, which costs nothing.
	turnCtx, turnSpan := startTurnSpan(ctx, state)
	turnCtx = withUsageScope(turnCtx, root.underlyingScope())

	handlerErr := h(turnCtx, root, opts.History, opts.Input)

	// Derive the final close reason
	state.mu.Lock()
	deriveCloseReason(state, ctx, handlerErr)
	state.mu.Unlock()

	// OTel: close the Turn, setting the span Status from the CloseReason and attaching the
	// total usage
	state.mu.Lock()
	finalizeTurnSpan(turnSpan, state)
	state.mu.Unlock()

	snapshot := buildTurnSnapshot(state)

	// The returned error is set only when the handler returned one itself and a cancelled
	// ctx did not cause it
	var retErr error
	if handlerErr != nil && !IsCancelError(handlerErr) {
		retErr = handlerErr
	}
	return snapshot, retErr
}

// deriveCloseReason is called with mu already held. The priority order:
//  1. closeReason already set, sealed by FinalAnswer → keep it
//  2. a strict-mode sinkErr  → {Failed, "agent_error", cause: sinkErr}
//  3. ctx.DeadlineExceeded   → {Cancelled, "timeout"}
//  4. ctx.Canceled with cause → {Cancelled, "user_cancel" / "host_shutdown" / "external_cancel"}
//  5. handlerErr != nil      → {Failed, "agent_error", cause: handlerErr}
//  6. handlerErr nil, no FinalAnswer → {Failed, "no_final_answer"}
func deriveCloseReason(state *turnState, ctx context.Context, handlerErr error) {
	if state.closeReason != nil {
		return
	}
	if state.sinkErr != nil {
		state.closeReason = &CloseReason{
			Code:    CloseCodeAgentError,
			Message: state.sinkErr.Error(),
			Cause:   state.sinkErr,
		}
		return
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		ctxCause := context.Cause(ctx)
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			state.closeReason = &CloseReason{
				Code:  CloseCodeTimeout,
				Cause: ctxCause,
			}
		} else {
			state.closeReason = &CloseReason{
				Code:  closeCodeFromContextCancelCause(ctxCause),
				Cause: ctxCause,
			}
		}
		return
	}
	if handlerErr != nil {
		if IsCancelError(handlerErr) {
			state.closeReason = &CloseReason{
				Code:  closeCodeFromCancelError(handlerErr),
				Cause: handlerErr,
			}
			return
		}
		// Narrow down a finish_reason error from the LLM protocol
		code := CloseCodeAgentError
		switch {
		case errors.Is(handlerErr, ErrContentFilter):
			code = CloseCodeContentFilter
		case errors.Is(handlerErr, ErrOutputTruncated):
			code = CloseCodeOutputTruncated
		}
		state.closeReason = &CloseReason{
			Code:    code,
			Message: handlerErr.Error(),
			Cause:   handlerErr,
		}
		return
	}
	// handlerErr nil with no FinalAnswer → no_final_answer
	state.closeReason = &CloseReason{
		Code: CloseCodeNoFinal,
	}
}

// buildTurnSnapshot builds the public Turn snapshot from the state, deep-copying Items so
// the two share no data.
// validationFailedTurn returns a minimal *Turn for Run's argument-validation failure path,
// which keeps Run's invariant that it always returns a non-nil *Turn. A caller, and a
// static analyser, therefore never has to worry about a nil turn outside an error path.
func validationFailedTurn(opts RunOptions, msg string) *Turn {
	now := time.Now()
	return &Turn{
		Index:          opts.TurnIndex,
		ConversationID: opts.ConversationID,
		Status:         TurnStatusFailed,
		CloseReason: &CloseReason{
			Code:    CloseCodeAgentError,
			Message: msg,
		},
		Metadata:  opts.Metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func buildTurnSnapshot(state *turnState) *Turn {
	state.mu.Lock()
	defer state.mu.Unlock()

	return &Turn{
		Index:          state.turnIdx,
		Path:           state.turnPath,
		ConversationID: state.conversationID,
		Items:          copyItems(state.items),
		Status:         statusFromCloseReason(state.closeReason),
		CloseReason:    state.closeReason,
		Usage:          state.totalUsage,
		Metadata:       state.metadata,
		CreatedAt:      state.createdAt,
		UpdatedAt:      time.Now(),
	}
}

// statusFromCloseReason derives a TurnStatus from a CloseReason.
func statusFromCloseReason(cr *CloseReason) TurnStatus {
	if cr == nil {
		return TurnStatusInProgress
	}
	status, _ := StatusFromCloseCode(cr.Code)
	return status
}

// StatusFromCloseCode maps a CloseReason.Code to a TurnStatus.
func StatusFromCloseCode(code CloseCode) (TurnStatus, bool) {
	switch code {
	case CloseCodeFinalAnswer:
		return TurnStatusCompleted, true
	case CloseCodeUserCancel, CloseCodeTimeout, CloseCodeHostShutdown, CloseCodeExternalCancel:
		return TurnStatusCancelled, true
	case CloseCodeAgentError,
		CloseCodeContentFilter,
		CloseCodeOutputTruncated,
		CloseCodeNoFinal,
		CloseCodeInternalError:
		return TurnStatusFailed, true
	default:
		return TurnStatusFailed, false
	}
}

// IsCancelError reports whether err represents cooperative cancellation.
func IsCancelError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTurnClosed)
}

func closeCodeFromCancelError(err error) CloseCode {
	if errors.Is(err, context.DeadlineExceeded) {
		return CloseCodeTimeout
	}
	if errors.Is(err, ErrTurnClosed) {
		return CloseCodeExternalCancel
	}
	return closeCodeFromContextCancelCause(err)
}

func closeCodeFromContextCancelCause(err error) CloseCode {
	if errors.Is(err, ErrHostShutdown) {
		return CloseCodeHostShutdown
	}
	if errors.Is(err, ErrExternalCancel) {
		return CloseCodeExternalCancel
	}
	return CloseCodeUserCancel
}

// copyItems deep-copies the Items tree, so a Turn the caller holds shares nothing with
// the state's internal data.
func copyItems(items []Item) []Item {
	if items == nil {
		return nil
	}
	out := make([]Item, len(items))
	for i, it := range items {
		out[i] = it
		out[i].Children = copyItems(it.Children)
	}
	return out
}
