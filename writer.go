package loom

import "context"

// Writer agent 业务方写出 Item 的通用接口。
//
// 三层接口:
//   - Writer:基础写入面(Turn 根和 Step 都实现)
//   - Step:Writer + 是嵌套容器(由 Writer.Step 的闭包返回)
//   - TurnWriter:Writer + Turn 根专属(FinalAnswer)
//
// 设计原则:
//   - **OOP 风格**:agent 拿到 Writer / Step / TurnWriter 对象后,直接调它的方法,
//     不暴露 ItemRef / ParentRef 等底层细节。
//   - **闭包风格**:Step 嵌套和流式 Stream 都用闭包,框架自动管 Close/Finish/Abort,
//     业务方不会漏关、不会写错 outcome。
//   - **类型分层约束**:FinalAnswer 仅在 TurnWriter 上;Step 上无 FinalAnswer,
//     编译期防止"在 Step 内写最终答案"这种错误。
//
// 错误约定:
//   - Write* 失败一般是 Sink 抖动,框架默认 swallow + OnSinkErr 回调,返回 nil。
//     StrictSink=true 时返回 error,agent 可选择中止。
//   - Sealed 后再写返回 ErrTurnClosed,agent 应静默退出。
type Writer interface {
	// Path 返回本 Writer 所在的完整路径,如 "turn[0].step[0].step[1]"。
	// 用于 log / 调试 / 业务方传给 helper。
	Path() string

	// ===== 一次性写入(立即 Completed) =====
	//
	// 所有方法接受 label 参数 — 人类可读短标签,可空("" 时 UI 用 Kind+Index 派生默认)。

	// WriteReasoning 写一段 LLM 推理(reasoning_content)。
	// 也用于业务方主动写的"过程笔记"(分析/总结/诊断等),UI 按 label 关键字或 path
	// 自行决定渲染样式 — 框架不规定子分类。
	WriteReasoning(ctx context.Context, label, text string) error

	// WriteToolCall 写一次工具调用记录。
	// 不实际执行工具 — 业务方负责调 tool.Invoke,再用 WriteToolResult 配对写结果。
	// (后续会提供 Writer.RunTool helper 一步到位。)
	WriteToolCall(ctx context.Context, label string, call ToolCall) error

	// WriteToolResult 写一次工具执行结果。
	// result.CallID 必须跟前面 WriteToolCall 的 ToolCall.CallID 配对。
	WriteToolResult(ctx context.Context, label string, result ToolResult) error

	// ===== 流式写入(闭包) =====

	// StreamReasoning 流式写 reasoning。
	// 也用于流式生成的长文本(草稿 / 修订 / 答案构思等)— 跟一次性 WriteReasoning 同源。
	// 闭包内通过 ReasoningStream 累加 chunk。
	// 闭包返 nil → 框架自动 Finish(用累积值或 SetFinalText 给的值);
	// 返 error → 框架自动 Abort,error 向上传递。
	StreamReasoning(ctx context.Context, label string, fn func(ReasoningStream) error) error

	// ===== sub flow 嵌套(闭包) =====

	// Step 开一个嵌套子步骤(代码编排 sub flow)。
	// 闭包内拿到 Step(is-a Writer),可以继续 Write* / Stream* / 嵌套 Step。
	//
	// 闭包接收的 ctx 是 step span 的子 ctx — 闭包内调 StreamLLMToStep /
	// runOneTool 等 helper 时,它们起的子 span 自动嵌在 step 之下。
	// **重要**:闭包内调任何接受 ctx 的函数(LLM / tool / DB)都应传 stepCtx
	// (而非外层捕获的 ctx),否则子 span 会挂到错误的父节点。
	//
	// 闭包返 nil → Step 标 Completed;
	// 返 ErrStepIncomplete → Step 标 Incomplete,错误**不向上传递**(被 step 吸收);
	// 返其它非 nil error → Step 标 Failed,error 向上传递。
	//
	// label 是 UI 展示标题,可为空(匿名作用域)。
	Step(ctx context.Context, label string, fn func(stepCtx context.Context, s Step) error) error
}

// Step is-a Writer,代表一个嵌套容器。
// 接口面跟 Writer 完全一致 — 没有显式 Close 方法,框架在 Writer.Step 的闭包结束时
// 自动 Close,outcome 由闭包返回值决定。
type Step interface {
	Writer
}

// TurnWriter is-a Writer + Turn 根专属能力(FinalAnswer)。
// 仅 Handler 收到的根 Writer 是 TurnWriter,Step 不是。
//
// FinalAnswer 写入后 Turn 进入 sealed 状态:后续任何 Write* / Stream* / Step
// 都返回 ErrTurnClosed。
type TurnWriter interface {
	Writer

	// FinalAnswer 写最终回答(立即 Completed)。每个 Turn 只能调一次,
	// 第二次调返回 ErrTurnClosed。
	FinalAnswer(ctx context.Context, text string) error

	// StreamFinalAnswer 流式写最终回答。
	// 闭包结束(返 nil)即 Turn 封口,后续写入返 ErrTurnClosed。
	// 闭包返 error → 不封口(business 可选择重试或直接 return)。
	StreamFinalAnswer(ctx context.Context, fn func(FinalAnswerStream) error) error
}
