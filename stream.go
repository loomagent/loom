package loom

import "context"

// ReasoningStream 流式 Reasoning 写入句柄。
//
// 在 Writer.StreamReasoning 的闭包内使用。闭包返 nil → 框架自动 Finish
// (用累积值或 SetFinalText 给的覆盖值);返 error → 框架自动 Abort。
//
// AppendText 失败由 Sink 错误处理策略决定(默认 swallow + OnSinkErr 回调);
// 业务方一般不必检查 AppendText 的 error,但接口保留 error 以便严格模式可用。
type ReasoningStream interface {
	// AppendText 累加一段增量文本(LLM stream 的一个 chunk)。
	// 空字符串是 no-op。
	AppendText(ctx context.Context, chunk string) error

	// SetFinalText 覆盖 Finish 时使用的最终文本。
	// 不调时默认用 AppendText 累积值。
	// 用途:LLM 末帧可能给出一个完整 text(跟累积片段拼出来略有差异),用 SetFinalText 替换。
	SetFinalText(text string)
}

// FinalAnswerStream 流式 FinalAnswer 写入句柄(Turn 收尾)。
// 语义同 ReasoningStream。
//
// 闭包正常结束 → Turn 进入 sealed 状态,后续 Write*/Open* 返回 ErrTurnClosed。
type FinalAnswerStream interface {
	AppendText(ctx context.Context, chunk string) error
	SetFinalText(text string)
}
