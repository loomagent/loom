// Package modelprobe observes a chat model's real API behavior and derives a
// provider-neutral Loom capability profile.
package modelprobe

import (
	"context"
	"time"

	"github.com/loomagent/loom"
)

// Builder constructs the same underlying provider model with synthetic
// capabilities. Probe uses this to bypass declared capability gates while
// testing actual behavior.
type Builder interface {
	Build(ctx context.Context, capabilities loom.ModelCapabilities) (loom.ChatModel, error)
}

// BuilderFunc adapts a function to Builder.
type BuilderFunc func(context.Context, loom.ModelCapabilities) (loom.ChatModel, error)

func (f BuilderFunc) Build(ctx context.Context, capabilities loom.ModelCapabilities) (loom.ChatModel, error) {
	return f(ctx, capabilities)
}

// Outcome is the result of one behavioral probe.
type Outcome string

const (
	OutcomePositive Outcome = "positive"
	OutcomeNegative Outcome = "negative"
	OutcomeError    Outcome = "error"
)

// ErrorDisposition tells Probe whether a request error is evidence that a
// requested feature is unsupported or is operationally inconclusive.
type ErrorDisposition string

const (
	ErrorInconclusive ErrorDisposition = "inconclusive"
	ErrorUnsupported  ErrorDisposition = "unsupported"
)

// ErrorClassifier classifies provider errors. It is only applied to explicit
// feature requests; failure of the default-behavior request is always
// inconclusive. A nil classifier treats every error as inconclusive.
type ErrorClassifier func(error) ErrorDisposition

// Check names are stable identifiers suitable for stored reports.
const (
	CheckReasoningDefault     = "reasoning.default_on"
	CheckReasoningEnable      = "reasoning.enable"
	CheckReasoningDisable     = "reasoning.disable"
	CheckStructuredJSONObject = "structured_output.json_object"
	CheckStructuredJSONSchema = "structured_output.json_schema"
)

// Evidence contains machine-readable observations from one request. Response
// content is truncated because reports are commonly persisted or logged.
type Evidence struct {
	ReasoningTokens  uint64            `json:"reasoning_tokens,omitempty"`
	ReasoningContent bool              `json:"reasoning_content,omitempty"`
	FinishReason     loom.FinishReason `json:"finish_reason,omitempty"`
	ResponsePreview  string            `json:"response_preview,omitempty"`
	Error            string            `json:"error,omitempty"`
}

// Check records one request and its interpretation. Positive means the
// behavior named by Name was observed; Negative means the request completed
// but that behavior was not observed; Error is operationally inconclusive.
type Check struct {
	Name       string               `json:"name"`
	Outcome    Outcome              `json:"outcome"`
	Effort     loom.ReasoningEffort `json:"effort,omitempty"`
	DurationMS int64                `json:"duration_ms"`
	Evidence   Evidence             `json:"evidence"`
}

// Coverage distinguishes a confirmed unsupported capability from a field that
// remains unknown because one or more requests failed.
type Coverage struct {
	ReasoningSupport         bool `json:"reasoning_support"`
	AcceptedReasoningEfforts bool `json:"accepted_reasoning_efforts"`
	StructuredOutput         bool `json:"structured_output"`
}

// ObservedCapabilities is the stable JSON representation derived by Probe.
// AcceptedReasoningEfforts means each parameter was accepted and produced
// observable reasoning; it does not claim statistically different budgets.
type ObservedCapabilities struct {
	Reasoning                loom.ReasoningSupport     `json:"reasoning"`
	AcceptedReasoningEfforts []loom.ReasoningEffort    `json:"accepted_reasoning_efforts"`
	StructuredOutput         loom.StructuredOutputMode `json:"structured_output"`
}

// Report is a serializable behavioral capability profile.
type Report struct {
	SchemaVersion int                  `json:"schema_version"`
	Model         string               `json:"model,omitempty"`
	Observed      ObservedCapabilities `json:"observed"`
	Coverage      Coverage             `json:"coverage"`
	Checks        []Check              `json:"checks"`
}

// Options controls probing. Calls are deliberately sequential to reduce rate
// limit pressure and make provider-side behavior easier to audit.
type Options struct {
	PerCallTimeout time.Duration
	// ReasoningEfforts defaults to low, medium, high, and max when nil.
	// A non-nil empty slice disables effort probing.
	ReasoningEfforts []loom.ReasoningEffort
	ErrorClassifier  ErrorClassifier
}

// Mismatch describes one confirmed difference between declared and observed
// capabilities.
type Mismatch struct {
	Field    string `json:"field"`
	Declared string `json:"declared"`
	Observed string `json:"observed"`
}
