package loom

import "time"

// ItemKind is the kind of an Item. Each kind uses a different set of fields; see the field
// comments on Item.
type ItemKind string

const (
	// ItemKindUserMessage is the user's question. One per Turn.
	// Fields: Text.
	ItemKindUserMessage ItemKind = "user_message"

	// ItemKindReasoning is the model's reasoning, its reasoning_content.
	// Fields: Text.
	ItemKindReasoning ItemKind = "reasoning"

	// ItemKindStep is a nested container, a code-orchestrated sub flow.
	// Fields: Label and Children.
	ItemKindStep ItemKind = "step"

	// ItemKindToolCall is a tool call the model asked for.
	// Fields: ToolName, ToolCallID, Arguments.
	ItemKindToolCall ItemKind = "tool_call"

	// ItemKindToolResult is the result of running a tool.
	// Fields: ToolName, ToolCallID, Output, Error.
	ItemKindToolResult ItemKind = "tool_result"

	// ItemKindFinalAnswer is one attempt at the final answer. A Turn commits at most one of
	// them — claiming and sealing the commit happen in one critical section, so a Turn cannot
	// end with two answers — but an attempt that failed stays in the tree as a candidate. Every
	// attempt therefore carries its own occurrence index and path.
	// Fields: Text. Committing one moves the Turn to Completed.
	ItemKindFinalAnswer ItemKind = "final_answer"
)

// ItemStatus is the lifecycle state of one Item.
type ItemStatus string

const (
	// ItemStatusInProgress is still streaming or otherwise open.
	ItemStatusInProgress ItemStatus = "in_progress"
	// ItemStatusCompleted finished normally.
	ItemStatusCompleted ItemStatus = "completed"
	// ItemStatusCancelled was cancelled by the user, a timeout, or the host. It is not
	// an error.
	ItemStatusCancelled ItemStatus = "cancelled"
	// ItemStatusFailed is a real error, so Error is required.
	ItemStatusFailed ItemStatus = "failed"
)

// ItemError is the structured error of a failed Item. A failed tool call, an
// interrupted reasoning block, and a failed step all use it.
type ItemError struct {
	// Code is a machine-readable error code, an open enum the product may extend.
	// Common values: "tool_failed", "llm_error", "validation_failed", and so on.
	Code string
	// Message is a human-readable description.
	Message string
}

// Item is one node inside a Turn: the user's input, part of the agent's output
// process, a step container, and so on.
//
// The design is deliberately one generic struct plus a Kind field rather than a
// sealed interface: JSON serialization goes straight to storage, other languages
// can read it, and a Sink reads fields without a type switch.
//
// Which fields a given Kind uses:
//   - user_message / reasoning / final_answer: Text
//   - step: Label and Children (nested items)
//   - tool_call: ToolName, ToolCallID, Arguments
//   - tool_result: ToolName, ToolCallID, Output, Error
//   - any Kind whose status is Failed: Error is required
//
// Children may only be non-empty when Kind=step.
type Item struct {
	// Kind decides which fields this Item uses.
	Kind ItemKind

	// Index is the 0-based position of this Kind under the same parent. The second
	// reasoning inside a step has Index=1.
	Index uint64

	// Path is the full path, such as "turn[0].step[1].reasoning[0]". The framework
	// derives it from the Turn index, the position in the Items tree, the Kind, and
	// Index.
	Path string

	// Status is this Item's lifecycle state.
	Status ItemStatus

	// ===== Kind-specific fields, of which only some are filled =====

	// Text is used by user_message, reasoning, final_answer, and note.
	Text string
	// MessageSource / MessagePurpose are only used by persisted user messages.
	MessageSource  MessageSource
	MessagePurpose MessagePurpose
	// Label is a human-readable label any Item may carry, and may be empty: a UI can
	// derive a default from the Kind and Index. A step uses it as its phase title;
	// reasoning, tool_call, and tool_result use it as a short description.
	Label string
	// ToolName is used by tool_call and tool_result.
	ToolName string
	// ToolCallID is used by tool_call and tool_result as the pairing identity the
	// model assigned.
	ToolCallID string
	// Arguments is used by tool_call: the tool's input as a JSON string.
	Arguments string
	// Output is used by tool_result: the tool's return value as a JSON string of
	// any structure.
	Output string
	// Error is set when the item failed. It is required for Failed and nil for any
	// other status.
	Error *ItemError

	// ===== Nesting (Kind=step only) =====

	// Children holds nested items. It may be non-empty only when Kind=step, and must
	// be nil otherwise. Nesting goes to any depth: a step contains a step to express
	// a sub flow.
	Children []Item

	// ===== Token accumulation (meaningful for Kind=step only) =====

	// Usage accumulates the token usage of every LLM call in this step and its
	// subtree. When an LLMCalledEvent fires, the framework walks the index chain of
	// the scope that triggered it up through each ancestor step's Usage, the turn
	// root included. Every other Kind leaves this at its zero value.
	Usage Usage

	// ===== Timestamps =====

	StartedAt time.Time
	UpdatedAt time.Time
}
