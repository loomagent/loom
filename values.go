package loom

// UserMessage is the user's input, the starting point of this turn.
//
// Only plain text is supported today. Multimodal support will add fields
// (Images / Files / ...) without changing what Text means, so existing callers
// keep working.
type UserMessage struct {
	Text    string
	Source  MessageSource
	Purpose MessagePurpose
}

// ToolResult is the outcome of running a tool, written through
// Writer.WriteToolResult or a stream closure.
//
// Notes:
//   - CallID must match ToolCall.CallID from the earlier WriteToolCall; it is the
//     pairing credential.
//   - ToolName must match ToolCall.Name of that call. Both the tool_call and the
//     tool_result row then carry a tool_name, which makes history queryable by
//     tool, as a citation loader scanning web_search output does. The framework
//     helpers fill it in; when you call WriteToolResult directly, fill it too.
//   - Output is the tool's return value as a JSON string. Its structure is decided
//     by the tool's own protocol.
//   - Err is non-nil when the tool failed. Give Output an error description in
//     JSON as well, so the model can read what happened.
type ToolResult struct {
	CallID   string
	ToolName string
	Output   string
	Err      *ItemError
}
