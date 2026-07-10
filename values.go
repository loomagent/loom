package loom

// UserMessage 用户输入(本轮 turn 的起点)。
//
// 当前仅支持纯文本;后续扩展多模态时新增字段(Images / Files / 等),
// 不改 Text 语义,保持向后兼容。
type UserMessage struct {
	Text string
}

// ToolResult 工具执行结果,用于 Writer.WriteToolResult / Stream 闭包写入。
//
// 注意:
//   - CallID 必须跟之前 WriteToolCall 里的 ToolCall.CallID 一致(配对凭据)。
//   - ToolName 跟对应 tool_call 的 ToolCall.Name 一致;让 DB 上 tool_call/tool_result
//     两行都带 tool_name,便于按工具维度查询历史结果(如 citation loader 扫
//     web_search 输出)。framework helper(ExecuteToolCalls / RunToolByName)自动填,
//     业务直接调 WriteToolResult 时也应填。
//   - Output 是工具返回值的 JSON 字符串(任意结构,由具体工具协议决定)。
//   - 工具失败时 Err 非 nil(也建议 Output 给一个错误描述 JSON 让 LLM 看懂)。
type ToolResult struct {
	CallID   string
	ToolName string
	Output   string
	Err      *ItemError
}
