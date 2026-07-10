package loom

import (
	"context"
	"errors"
	"time"
)

// Handler agent 业务方实现的核心函数。
//
// 通过 TurnWriter 写出过程事件(Step / Reasoning / ToolCall / ... / FinalAnswer)。
// 返回 error 表达失败:
//   - nil + 调过 FinalAnswer   → Turn Completed
//   - nil + 没调 FinalAnswer    → Turn Failed(code=no_final_answer)
//   - 非 nil                    → Turn Failed(code=agent_error,cause=err)
//
// ctx 的 cancel / deadline 由 Run 检测,转成对应的 Cancelled / 超时终态。
type Handler func(
	ctx context.Context,
	w TurnWriter,
	history []Turn,
	input UserMessage,
) error

// RunOptions Run 的所有配置。
type RunOptions struct {
	// Sinks 事件下游。可空(开发期调试场景),Run 内核 fan-out 给所有 sink。
	// 多个 Sink 直接传 []Sink,无需 Tee 包装。
	//
	// 业务方典型组合:EntSink(落库)+ LogSink(打 log)+ WSSink(推前端)+ ...,
	// 各 Sink 独立处理自己关心的事件(详见 Sink.ItemDelta 的"按需消费"语义)。
	Sinks []Sink

	// History 已完成的历史 Turn(按时间升序),拼上下文用。
	// 由调用方加载(loom 不规定历史从哪来 — 业务方可能查 DB / 缓存 / 内存)。
	History []Turn

	// Input 本轮用户输入,Run 自动作为第一个 item 写入(user_message)。
	Input UserMessage

	// ConversationID 本轮归属的 conversation id,**必填**(空字符串 Run 返错)。
	// 一等公民概念:
	//   - 写到 OTel turn span 的 loom.conversation.id attribute,方便后端按对话聚合多轮 trace
	//   - 写到 Turn struct 数据快照,业务方 Repository/Sink 直接拿,不需要解 metadata
	//   - 业务侧加载跨轮状态(citation src_id 历史扫描等)用同样的 ID
	ConversationID string

	// TurnIndex 在 conversation 内的 0-based 序号。
	// 0 默认值会被解读为"自动 = uint64(len(History))",
	// 业务方分页加载历史(History 不完整)时应显式传非 0 值。
	TurnIndex uint64

	// Metadata 业务侧透传 K/V(loom 不解释)。
	// 常见 key:"conversation_id" / "user_id" / "chat_mode" / ...
	Metadata map[string]string

	// OnSinkErr Sink 失败回调(默认 nil = 静默 swallow)。
	OnSinkErr func(Sink, error)

	// StrictSink true 时任一 Sink 失败立即让整 Turn Failed。
	// 默认 false:Sink 失败不影响主流程,继续跑。
	StrictSink bool

	// CaptureContent 控制 OTel span 是否落 prompt / completion / tool args/output。
	// false(默认)只保留元数据(model / token / finish_reason / latency),
	// 适合生产含 PII / 合规敏感数据时。开发期建议设 true 方便调试。
	CaptureContent bool
}

// Run 执行一次 agent。
//
// 流程:
//  1. 构造 turnState + turnRoot,注册 Sinks
//  2. 调 handler(ctx, root, History, Input)
//  3. 根据 handler 返回值 + state.closeReason + ctx 状态 + state.sinkErr,
//     派生最终 CloseReason
//  4. 返回 *Turn 数据快照 + error
//     - error 仅在 handler return 非 nil(且不是 ctx 取消)时返回
//     - 正常完成 / 取消 / no_final_answer 等返 nil error,详情看 Turn.CloseReason
//
// 注:loom.Run **不自动写 user_message Item**。
// user_message 的持久化由调用方(如 wolosink.InsertUserMessageItem)在 Run 之前完成,
// 然后通过 opts.Input 把 input 文本传给 handler 使用。
// 返回的 *Turn.Items 不含 user_message — 完整 items 树请从持久化层加载。
func Run(ctx context.Context, h Handler, opts RunOptions) (*Turn, error) {
	if h == nil {
		return validationFailedTurn(opts, "handler 不能为空"), errors.New("loom.Run: handler 不能为空")
	}
	if opts.ConversationID == "" {
		return validationFailedTurn(opts, "ConversationID 必填"), errors.New("loom.Run: ConversationID 必填")
	}

	turnIdx := opts.TurnIndex
	if turnIdx == 0 {
		turnIdx = uint64(len(opts.History))
	}
	state := newTurnState(turnIdx, opts.ConversationID, opts.Sinks, opts.OnSinkErr, opts.StrictSink, opts.Metadata, opts.CaptureContent)
	root := newTurnRoot(state)

	// OTel:起 Turn 根 span,handler 拿到的 ctx 已嵌入 span,Step / StreamLLMToStep /
	// runOneTool 内部起的子 span 自然成为子节点。OTel 未 Setup 时是 noop tracer,零开销。
	turnCtx, turnSpan := startTurnSpan(ctx, state)

	handlerErr := h(turnCtx, root, opts.History, opts.Input)

	// 派生最终 closeReason
	state.mu.Lock()
	deriveCloseReason(state, ctx, handlerErr)
	state.mu.Unlock()

	// OTel:Turn 收尾 — 按 CloseReason 设 span Status,打上 totalUsage 总和
	state.mu.Lock()
	finalizeTurnSpan(turnSpan, state)
	state.mu.Unlock()

	snapshot := buildTurnSnapshot(state)

	// 返回的 error:仅 handler 主动 return 非 nil 时(且不是 ctx 取消触发)
	var retErr error
	if handlerErr != nil && !IsCancelError(handlerErr) {
		retErr = handlerErr
	}
	return snapshot, retErr
}

// deriveCloseReason mu 已持有时调。
// 优先级:
//  1. closeReason 已设(FinalAnswer 自封口)→ keep
//  2. strict 模式 sinkErr        → {Failed, "agent_error", cause: sinkErr}
//  3. ctx.DeadlineExceeded       → {Cancelled, "timeout"}
//  4. ctx.Canceled + Cause       → {Cancelled, "user_cancel"/"host_shutdown"/"external_cancel"}
//  5. handlerErr != nil          → {Failed, "agent_error", cause: handlerErr}
//  6. handlerErr nil + 没 FinalAnswer → {Failed, "no_final_answer"}
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
		// 细分 LLM 协议层的 finish_reason 错误
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
	// handlerErr nil + 没 FinalAnswer → no_final_answer
	state.closeReason = &CloseReason{
		Code: CloseCodeNoFinal,
	}
}

// buildTurnSnapshot 从 state 构造对外的 Turn 快照(Items 深拷贝隔离)。
// validationFailedTurn 给 Run 的入参校验失败路径返回一个最小 *Turn,
// 保证 Run 的不变量"永远返回非 nil *Turn"。让调用方(以及静态分析器 nilaway)
// 不用在 nil err 路径之外还要担心 turn 为 nil。
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

// statusFromCloseReason CloseReason → TurnStatus 派生。
func statusFromCloseReason(cr *CloseReason) TurnStatus {
	if cr == nil {
		return TurnStatusInProgress
	}
	status, _ := StatusFromCloseCode(cr.Code)
	return status
}

// StatusFromCloseCode 把 CloseReason.Code 映射为 TurnStatus。
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

// IsCancelError 判断 err 是否代表协作式取消。
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

// copyItems 深拷贝 Items 树(避免业务方持有的 Turn 跟 state 内部数据共享)。
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
