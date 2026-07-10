package loom

import "time"

// ItemKind Item 的种类。每个 Kind 用到的字段集合不同,见 Item struct 的字段注释。
type ItemKind string

const (
	// ItemKindUserMessage 用户提问。每个 Turn 单例。
	// 字段:Text。
	ItemKindUserMessage ItemKind = "user_message"

	// ItemKindReasoning LLM 推理过程(reasoning_content)。
	// 字段:Text。
	ItemKindReasoning ItemKind = "reasoning"

	// ItemKindStep 嵌套容器(代码编排 sub flow)。
	// 字段:Label + Children。
	ItemKindStep ItemKind = "step"

	// ItemKindToolCall LLM 发起的工具调用。
	// 字段:ToolName / ToolCallID / Arguments。
	ItemKindToolCall ItemKind = "tool_call"

	// ItemKindToolResult 工具执行结果。
	// 字段:ToolName / ToolCallID / Output / Error。
	ItemKindToolResult ItemKind = "tool_result"

	// ItemKindFinalAnswer 最终回答。每个 Turn 单例。
	// 字段:Text。Turn 状态机:写入后 Turn 标 Completed。
	ItemKindFinalAnswer ItemKind = "final_answer"
)

// ItemStatus 单个 Item 的生命周期状态。
type ItemStatus string

const (
	// ItemStatusInProgress 流式中 / 未闭合。
	ItemStatusInProgress ItemStatus = "in_progress"
	// ItemStatusCompleted 正常完成。
	ItemStatusCompleted ItemStatus = "completed"
	// ItemStatusCancelled 被取消(用户主动 / 超时 / host 取消),不是错误。
	ItemStatusCancelled ItemStatus = "cancelled"
	// ItemStatusFailed 真错误(Error 字段必填)。
	ItemStatusFailed ItemStatus = "failed"
)

// ItemError Item 失败时的结构化错误。
// Tool 调用失败、Reasoning 中断、Step 失败等都用此结构。
type ItemError struct {
	// Code 机器可识别的错误码(open enum,业务可扩)。
	// 常见:"tool_failed" / "llm_error" / "validation_failed" / ...
	Code string
	// Message 人类可读错误描述。
	Message string
}

// Item Turn 内的一个节点(用户输入 / agent 输出过程 / step 容器 / ...)。
//
// 设计原则:**通用 struct + Kind 字段**,不用 sealed interface。
// 优势:JSON 序列化直接落库 / 跨语言友好 / Sink 实现读字段不用 type switch。
//
// 字段使用约定(按 Kind 分):
//   - user_message / reasoning / final_answer:Text
//   - step:Label + Children(嵌套子 item)
//   - tool_call:ToolName / ToolCallID / Arguments
//   - tool_result:ToolName / ToolCallID / Output / Error
//   - failed item(任意 Kind 状态=Failed):Error 必填
//
// 仅 Kind=step 时 Children 才允许非空。
type Item struct {
	// Kind 决定本 Item 用哪些字段。
	Kind ItemKind

	// Index 同 parent 下同 Kind 的 0-based 下标。
	// 例:某 step 内的第 2 个 reasoning,Index=1。
	Index uint64

	// Path 完整路径,如 "turn[0].step[1].reasoning[0]"。
	// 派生自 (Turn.Index, 在 Items 树中的位置, Kind, Index),由框架填充。
	Path string

	// Status Item 生命周期状态。
	Status ItemStatus

	// ===== Kind-specific 字段(只填一部分,按 Kind 决定) =====

	// Text user_message / reasoning / final_answer / note 用。
	Text string
	// Label 所有 Item 通用的人类可读标签(可空 — UI 渲染时自行用 Kind+Index 派生默认)。
	// step 用作阶段标题,reasoning / tool_call / tool_result 用作子项短描述。
	Label string
	// ToolName tool_call / tool_result 用。
	ToolName string
	// ToolCallID tool_call / tool_result 用,配对标识(LLM 分配)。
	ToolCallID string
	// Arguments tool_call 用,工具入参(JSON 字符串)。
	Arguments string
	// Output tool_result 用,工具返回(JSON 字符串,任意结构)。
	Output string
	// Error item 失败时填(Status=Failed 必填,其它状态留 nil)。
	Error *ItemError

	// ===== 嵌套(仅 Kind=step) =====

	// Children 嵌套子 items。仅 Kind=step 时允许非空,其它 Kind 必须 nil。
	// 任意深度嵌套 — step 可嵌套 step 表达 sub flow。
	Children []Item

	// ===== Token 累计(仅 Kind=step 有意义) =====

	// Usage 本 step 及其子树下所有 LLM 调用的累计 token 用量。
	// LLMCalledEvent 触发时,框架沿触发 scope 的 indices 链向上累加到各祖先 step
	// 的 Usage(也累加到 turnState 根)。
	// 其它 Kind 该字段保持零值。
	Usage Usage

	// ===== 时间戳 =====

	StartedAt time.Time
	UpdatedAt time.Time
}
