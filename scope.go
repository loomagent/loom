package loom

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// turnState 一次 Turn 执行的全局共享状态。
//
// 由 turn 根和所有 step 共享同一份指针,保证:
//   - closeReason 全局可见(任一来源翻成非 nil 后,所有 scope 拒绝写入)
//   - items 树是单一权威数据(各 scope 通过 childrenPtr 指向不同子树)
//   - Sink 调用串行化(共享 mu)
//
// 所有可变字段访问必须持有 mu。
type turnState struct {
	mu sync.Mutex

	// ===== 不变字段(构造时设定) =====
	turnIdx        uint64
	turnPath       string // "turn[0]"
	conversationID string // 一等公民,Run 入口校验非空

	sinks      []Sink
	onSinkErr  func(Sink, error)
	strictSink bool
	metadata   map[string]string

	// captureContent 控制 OTel span 是否落 prompt / completion / tool args/output。
	// false 时只保留元数据(model / token / finish_reason / latency),适合生产含 PII / 合规敏感数据时。
	// 从 RunOptions.CaptureContent 透传。
	captureContent bool

	// ===== 可变字段(mu 保护) =====

	// closeReason nil = 仍在运行;非 nil = 已封口。
	// 非 nil 时 Status 可由 CloseReason.Code 派生为 Completed / Cancelled / Failed。
	// 翻成非 nil 的来源:
	//   - FinalAnswer / StreamFinalAnswer 成功 → {Completed, "final_answer"}
	//   - 外部 cancel(ctx.Cancel)              → {Cancelled, "user_cancel"}
	//   - 超时(ctx.DeadlineExceeded)            → {Cancelled, "timeout"}
	//   - handler return error                 → {Failed, "agent_error" / "content_filter" / ...}
	//   - Sink strict 模式失败                  → {Failed, "agent_error"} + Cause=sinkErr
	closeReason *CloseReason

	// items Turn.Items 根列表(嵌套树通过 Item.Children)。
	items []Item

	// sinkErr strict 模式下记录第一次 Sink 失败,Run 收尾时检测让 Turn fail。
	sinkErr error

	// callIDCounter loom 自动生成 tool callID 时的 0-based 计数器(turn 内递增)。
	// LLM 触发场景(ExecuteToolCalls)用 LLM 给的 ToolCall.ID,不动此 counter;
	// 代码编排场景(RunToolByName)用此 counter 生成 "call_0" / "call_1" / ...
	callIDCounter uint64

	// totalUsage 本 Turn 所有 LLM 调用的累计 token 用量。
	// emitLLMCalled 锁内累加(turn 根的等价物 — 已经累加到每个祖先 step.Usage 后,
	// 同时累加这个总和)。Run 收尾时由 buildTurnSnapshot 拷到 Turn.Usage。
	totalUsage Usage

	// createdAt Turn 开始时间(Run 收尾构造 Turn 快照用)。
	createdAt time.Time
}

// newTurnState 构造 turnState(供 Run 调用,不导出)。
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

// isClosed mu 已持有时调用。
// true 表示 Turn 已封口(任何写入应返 ErrTurnClosed)。
func (s *turnState) isClosed() bool {
	return s.closeReason != nil
}

// nextToolCallIDLocked mu 已持有时调,生成 "call_N" 形式的递增 callID。
// 仅 helper(RunToolByName 等)代码编排场景使用;LLM 触发场景直接用 ToolCall.ID。
func (s *turnState) nextToolCallIDLocked() string {
	id := "call_" + strconv.FormatUint(s.callIDCounter, 10)
	s.callIDCounter++
	return id
}

// formatTurnPath 把 turn index 转成 "turn[N]"。
func formatTurnPath(idx uint64) string {
	return "turn[" + strconv.FormatUint(idx, 10) + "]"
}

// writerScope 一个 Writer/Step 所在的作用域。
// Turn 根和任意 step 都用同一类型;区别在 indices 链(从 turn.items 根到本 scope 的路径)。
//
// 所有方法(写入 / 嵌套)通过 state.mu 串行化,scope 自身不带锁。
//
// 并发约定:同一 scope 不可跨 goroutine 并发使用;父子 scope 不可同时使用
// (子 step 闭包内不操作 outer scope)。违反约定是 undefined behavior。
type writerScope struct {
	state *turnState // 指向 Turn 全局状态(共享引用)

	// path 完整路径,如 "turn[0].step[0].step[1]"。
	// Turn 根时等于 state.turnPath。
	path string

	// indices 从 state.items 根开始定位本 scope 的索引链:
	//   - root scope:nil(children = &state.items)
	//   - 子 scope:[a, b, c] → children = &state.items[a].Children[b].Children[c].Children
	//
	// 每次写入都重走索引(locateChildrenLocked),避免切片扩容导致缓存地址失效。
	indices []int

	// counters 同 scope 下各 ItemKind 的 0-based 计数器。
	// 写入新 item 时根据 kind 取当前值作为 Item.Index,然后递增。
	counters map[ItemKind]uint64
}

// newRootScope 构造 Turn 根 scope(由 turnRoot 持有)。
func newRootScope(state *turnState) *writerScope {
	return &writerScope{
		state:    state,
		path:     state.turnPath,
		counters: map[ItemKind]uint64{},
		indices:  nil,
	}
}

// locateChildrenLocked 沿 indices 链定位本 scope 的 children 切片。
// state.mu 必须已持有。每次调都重走索引 — 切片扩容后地址仍正确。
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

// Path 返回本 scope 的完整路径,如 "turn[0].step[0]"。
func (w *writerScope) Path() string {
	return w.path
}

// underlyingScope 实现 scopeAccessor(包内 helper 用,如 RunToolByName 取 turnState)。
// step / turnRoot 内嵌 *writerScope,自动继承此方法。
func (w *writerScope) underlyingScope() *writerScope {
	return w
}

// ===== Sink fan-out helpers =====
// 这些方法不持 mu(调用方在 mu 释放后才调,避免 Sink 耗时阻塞同 Turn 其它写入)。

// emitItemStarted 广播 ItemStartedEvent 给所有 sink。
func (s *turnState) emitItemStarted(ctx context.Context, item Item) {
	ev := ItemStartedEvent{
		TurnIndex: s.turnIdx,
		TurnPath:  s.turnPath,
		Item:      item,
		Time:      time.Now(),
	}
	s.fanOut(ctx, func(sink Sink) error { return sink.ItemStarted(ctx, ev) })
}

// emitItemDelta 广播 ItemDeltaEvent 给所有 sink。
func (s *turnState) emitItemDelta(ctx context.Context, itemPath string, ch DeltaChannel, chunk string) {
	ev := ItemDeltaEvent{
		TurnIndex: s.turnIdx,
		TurnPath:  s.turnPath,
		ItemPath:  itemPath,
		Channel:   ch,
		Chunk:     chunk,
		Time:      time.Now(),
	}
	s.fanOut(ctx, func(sink Sink) error { return sink.ItemDelta(ctx, ev) })
}

// emitItemFinished 广播 ItemFinishedEvent 给所有 sink。
func (s *turnState) emitItemFinished(ctx context.Context, item Item) {
	ev := ItemFinishedEvent{
		TurnIndex: s.turnIdx,
		TurnPath:  s.turnPath,
		Item:      item,
		Time:      time.Now(),
	}
	finishCtx := context.WithoutCancel(ctx)
	s.fanOut(finishCtx, func(sink Sink) error { return sink.ItemFinished(finishCtx, ev) })
}

// emitLLMCalled 把一次 LLM 调用的 usage 沿 indices 链向上累加到祖先 step Item.Usage,
// 然后 fan-out LLMCalledEvent 给所有 sink。
//
// 累加路径:从 indices[0] 开始,逐级深入 items 树,沿途凡是 Kind=step 的祖先都加。
// LLM 调用挂在 turn 根(indices=nil)时也调本方法,此时只 fan-out,没有 step 累加。
//
// stepPath 取 scope.path(turn 根时与 turnPath 相同,fan-out 给 sink 时翻成空串)。
// model / purpose 透传到 sink,供观测用。
//
// 串行约定:跟 emitItem* 一样,fan-out 在锁外进行,Sink.LLMCalled 不能依赖 mu 已持有。
func (s *turnState) emitLLMCalled(ctx context.Context, scope *writerScope, model, purpose string, usage Usage) {
	s.mu.Lock()
	if !s.isClosed() {
		// 沿 indices 链向上累加各 step item.Usage(turn 根 indices 为空 → 跳过)
		container := &s.items
		for _, idx := range scope.indices {
			addUsage(&(*container)[idx].Usage, usage)
			container = &(*container)[idx].Children
		}
		// turn 根累计
		addUsage(&s.totalUsage, usage)
	}
	s.mu.Unlock()

	stepPath := scope.path
	if stepPath == s.turnPath {
		stepPath = "" // turn 根
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
	s.fanOut(ctx, func(sink Sink) error { return sink.LLMCalled(ctx, ev) })
}

// addUsage 把 b 累加到 a(各字段独立 +)。
func addUsage(a *Usage, b Usage) {
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.CachedTokens += b.CachedTokens
	a.ReasoningTokens += b.ReasoningTokens
	a.TotalTokens += b.TotalTokens
}

// fanOut 同步串行调所有 sink。任一失败:
//   - 调用 onSinkErr 回调(若设置)
//   - strict 模式下记录第一次失败到 state.sinkErr(Run 收尾据此让 Turn fail)
func (s *turnState) fanOut(ctx context.Context, fn func(Sink) error) {
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

// ===== 一次性写入方法 =====

// WriteReasoning 写一段 LLM 推理(立即 Completed)。
func (w *writerScope) WriteReasoning(ctx context.Context, label, text string) error {
	return w.writeSimpleItem(ctx, ItemKindReasoning, func(it *Item) {
		it.Label = label
		it.Text = text
	})
}

// WriteToolCall 写一次工具调用记录(立即 Completed)。
// 不实际执行工具 — 业务方自己调 tool,然后用 WriteToolResult 配对写结果。
func (w *writerScope) WriteToolCall(ctx context.Context, label string, call ToolCall) error {
	return w.writeSimpleItem(ctx, ItemKindToolCall, func(it *Item) {
		it.Label = label
		it.ToolName = call.Name
		it.ToolCallID = call.ID
		it.Arguments = call.Arguments
	})
}

// WriteToolResult 写一次工具执行结果。
// result.CallID 必须跟前面 WriteToolCall 的 ToolCall.CallID 配对。
// result.ToolName 同对应 tool_call 的 Name(让 DB 上 tool_call/tool_result 对称,
// 便于按工具维度查询历史输出,如 citation loader 扫 web_search)。
// result.Err 非 nil 时 Status=Failed,否则 Completed。
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

// ===== 流式写入(闭包) =====

// StreamReasoning 流式写一段 LLM 推理。
//
// 闭包语义:
//   - 返 nil    → Status=Completed,Text=SetFinalText(若设)或 accumText 累积值
//   - 返 非 nil → Status=Failed,Error 填,Text=累积值(半成品保留)
func (w *writerScope) StreamReasoning(ctx context.Context, label string, fn func(ReasoningStream) error) error {
	selfIdx, item, ok := w.openStreamItem(ItemKindReasoning, label, func(it *Item) {
		it.Label = label
	})
	if !ok {
		return ErrTurnClosed
	}
	w.state.emitItemStarted(ctx, item)

	stream := &reasoningStream{state: w.state, itemPath: item.Path}
	fnErr := fn(stream)

	snapshot := w.finalizeStreamItem(selfIdx, stream.core.finalize(), fnErr)
	w.state.emitItemFinished(ctx, snapshot)
	return fnErr
}

// openStreamItem 流式写入"开"阶段共用:
//   - check sealed
//   - 分配 index/path
//   - 构造 Item(Status=InProgress) + append 到 children
//   - 返回 selfIdx(供 finalize 时 locate 用)+ item 快照
//
// ok=false 表示 sealed,调用方应返 ErrTurnClosed。
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

// finalizeStreamItem 流式写入"关"阶段共用:
// 根据 fnErr 决定 Status,更新 Text/Status/Error,返回最终快照。
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

// step 实现 Step 接口(is-a Writer)。Writer 方法全部继承 writerScope。
type step struct {
	*writerScope
}

// 编译期接口断言。
var (
	_ Writer = (*writerScope)(nil)
	_ Step   = (*step)(nil)
)

// Step 在当前 scope 下开一个嵌套子 step。
//
// 闭包语义:
//   - 返 nil      → step Close(Completed)
//   - 返 非 nil   → step Close(Failed,Error 字段填),error 向上传递
//
// 闭包执行期间不持 state.mu,业务方可以放心做长耗时操作(LLM 调用等)。
func (w *writerScope) Step(ctx context.Context, label string, fn func(context.Context, Step) error) error {
	// 1. 加锁,check sealed,分配 step item
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

	// 子 scope 的 indices = 父 indices + selfIdx(复制,不共享底层数组)
	childIndices := make([]int, len(w.indices)+1)
	copy(childIndices, w.indices)
	childIndices[len(w.indices)] = selfIdx

	w.state.mu.Unlock()

	// 2. emit Started
	w.state.emitItemStarted(ctx, item)

	// 3. OTel:起 step 子 span,闭包内 ctx 已嵌入子 span。
	// 嵌套 step 闭包内调 StreamLLMToStep / runOneTool 时自然成为子节点。
	stepCtx, stepSpan := startStepSpan(ctx, path, label)

	// 4. 构造 child scope 跑闭包
	childScope := &writerScope{
		state:    w.state,
		path:     path,
		indices:  childIndices,
		counters: map[ItemKind]uint64{},
	}
	fnErr := fn(stepCtx, &step{writerScope: childScope})

	// 5. OTel:闭包返回后收尾 span(放在 Item 状态更新之前 — span End 不阻塞同步路径)
	finalizeStepSpan(stepSpan, fnErr)

	// 4. 收尾:locate 父 children 重新拿地址,改 Status/Error
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

// writeSimpleItem 一次性写入的共用骨架(Reasoning/Note/ToolCall/ToolResult)。
//
// 流程:check sealed → 分配 index/path → 构造 Item(Status=Completed) →
// append 到 childrenPtr → 释放 mu → emit Started + Finished。
//
// fillKind 在 mu 持有时调,填充 Kind-specific 字段。
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

	// 锁外 emit。一次性 Item 同一份 payload 发 Started + Finished 两帧。
	w.state.emitItemStarted(ctx, item)
	w.state.emitItemFinished(ctx, item)
	return nil
}

// formatChildPath 拼子 item 路径。
// 单例 kind(user_message / final_answer)不带 index;其它带 "[N]"。
func formatChildPath(parentPath string, kind ItemKind, idx uint64) string {
	if isSingletonKind(kind) {
		return parentPath + "." + string(kind)
	}
	return parentPath + "." + string(kind) + "[" + strconv.FormatUint(idx, 10) + "]"
}

// isSingletonKind 返回此 kind 是否每个 turn 只能有一个。
// 单例 kind 由状态机保证不会被写第二次(WriteFinalAnswer 等内部 check)。
func isSingletonKind(kind ItemKind) bool {
	return kind == ItemKindUserMessage || kind == ItemKindFinalAnswer
}
