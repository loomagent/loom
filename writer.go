package loom

import "context"

// Writer is the general interface product code uses to write Items.
//
// Three layers:
//   - Writer: the base writing surface, implemented by both the Turn root and Step
//   - Step: a Writer that is also a nested container, returned by Writer.Step's closure
//   - TurnWriter: a Writer plus what only the Turn root can do, FinalAnswer
//
// Design principles:
//   - **OOP style**: agent code receives a Writer / Step / TurnWriter object and
//     calls its methods; lower-level details such as ItemRef and ParentRef stay
//     hidden.
//   - **Closure style**: nested Steps and streams both use closures, so the
//     framework manages Close/Finish/Abort and product code cannot forget to close
//     or record the wrong outcome.
//   - **Layered by type**: FinalAnswer exists only on TurnWriter. A Step has none,
//     so writing a final answer from inside a step does not compile.
//
// Errors:
//   - A failed Write* is usually a sink hiccup, so by default it is swallowed and
//     reported through OnSinkErr, and the method returns nil. With StrictSink=true
//     it returns the error and the agent may choose to stop.
//   - Writing after the Turn is sealed returns ErrTurnClosed, and the agent should
//     exit quietly.
type Writer interface {
	// Path returns the full path this Writer sits at, such as
	// "turn[0].step[0].step[1]". Use it for logging and debugging, or pass it to a
	// helper.
	Path() string

	// ===== One-shot writes, Completed immediately =====
	//
	// Every method takes a label: a short human-readable tag that may be empty. A UI
	// derives a default from the Kind and Index when it is.

	// WriteReasoning writes one block of model reasoning, its reasoning_content. It
	// also carries process notes product code writes deliberately, such as an
	// analysis, a summary, or a diagnosis. A UI decides how to render them from the
	// label or the path; the framework does not define subcategories.
	WriteReasoning(ctx context.Context, label, text string) error

	// WriteToolCall writes a record of one tool call. It does not run the tool:
	// product code calls tool.Invoke and then pairs the outcome with
	// WriteToolResult.
	WriteToolCall(ctx context.Context, label string, call ToolCall) error

	// WriteToolResult writes the result of one tool run. result.CallID must pair with
	// ToolCall.CallID from the earlier WriteToolCall.
	WriteToolResult(ctx context.Context, label string, result ToolResult) error

	// ===== Streaming writes (closures) =====

	// StreamReasoning streams reasoning. It also carries long text produced
	// incrementally, such as a draft, a revision, or a sketch of an answer, and is
	// the same kind of item as a one-shot WriteReasoning. Inside the closure,
	// ReasoningStream accumulates chunks. A nil return finishes the item with the
	// accumulated value or the one given to SetFinalText; an error aborts it and
	// propagates.
	StreamReasoning(ctx context.Context, label string, fn func(ReasoningStream) error) error

	// ===== Nested sub flows (closures) =====

	// Step opens a nested sub-step, a code-orchestrated sub flow. Inside the closure
	// you get a Step, which is a Writer, and can keep writing, streaming, and
	// nesting.
	//
	// The ctx the closure receives is a child of the step's span, so helpers called
	// inside it start their spans underneath the step. **Important**: pass that
	// stepCtx, not an outer captured ctx, to anything that takes a context (an LLM
	// call, a tool, a database query), or its child span hangs off the wrong parent.
	//
	// A nil return marks the Step Completed. A cancellation error marks it Cancelled and
	// still propagates. Any other non-nil error marks it Failed and propagates.
	//
	// label is the title a UI shows, and may be empty for an anonymous scope.
	Step(ctx context.Context, label string, fn func(stepCtx context.Context, s Step) error) error
}

// Step is a Writer representing a nested container. Its surface is exactly
// Writer's: there is no explicit Close, because the framework closes it when
// Writer.Step's closure ends, and the closure's return value decides the outcome.
type Step interface {
	Writer
}

// TurnWriter is a Writer plus what only the Turn root can do: the final answer.
// The root Writer a Handler receives is a TurnWriter; a Step is not.
//
// Once the final answer is written the Turn is sealed, and every later Write*,
// Stream*, or Step returns ErrTurnClosed.
type TurnWriter interface {
	Writer

	// FinalAnswer writes the final answer, Completed at once. It may be called once
	// per Turn; the second call returns ErrTurnClosed.
	FinalAnswer(ctx context.Context, text string) error

	// StreamFinalAnswer streams the final answer. A nil return from the closure seals
	// the Turn, and later writes return ErrTurnClosed. An error leaves it unsealed, so
	// product code may retry or return.
	StreamFinalAnswer(ctx context.Context, fn func(FinalAnswerStream) error) error
}
