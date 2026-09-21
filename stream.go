package loom

import "context"

// ReasoningStream is the handle for streaming reasoning.
//
// It is used inside the closure passed to Writer.StreamReasoning. A nil return
// from the closure finishes the item with the accumulated text or with the value
// given to SetFinalText; an error aborts it.
//
// A failed AppendText is governed by the Sink error policy, which swallows it by
// default and reports it through OnSinkErr. Callers normally need not check the
// error it returns; the interface keeps one so strict mode has it to use.
type ReasoningStream interface {
	// AppendText appends one increment of text: a single chunk of the model stream.
	// An empty string is a no-op.
	AppendText(ctx context.Context, chunk string) error

	// SetFinalText overrides the text used when the item finishes. Without it the
	// value accumulated by AppendText is used.
	//
	// The last frame may carry a complete text that differs slightly from the
	// concatenated increments; SetFinalText replaces the accumulated value with it.
	SetFinalText(text string)
}

// FinalAnswerStream is the handle for streaming the final answer that closes a
// Turn. It behaves exactly like ReasoningStream.
//
// When its closure ends normally the Turn becomes sealed, and later Write* or
// Open* calls return ErrTurnClosed.
type FinalAnswerStream interface {
	AppendText(ctx context.Context, chunk string) error
	SetFinalText(text string)
}
