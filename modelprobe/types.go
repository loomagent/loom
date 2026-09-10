// Package modelprobe observes a chat model's real API behavior and derives a
// provider-neutral Loom capability profile.
package modelprobe

import (
	"context"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

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

// ErrorClassifier classifies server-side provider errors. Local adapter validation
// errors are always inconclusive, even when the classifier says unsupported.
// It is only applied to explicit
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
	// Requested constraints are intent, not proof of adapter serialization or
	// server enforcement. Retain randomized schemas so results are auditable.
	RequestedResponseFormat string             `json:"requested_response_format,omitempty"`
	RequestedSchema         *jsonschema.Schema `json:"requested_schema,omitempty"`
	ResponseModel           string             `json:"response_model,omitempty"`
	// Acceptance is independent of observable reasoning and native semantics.
	Acceptance          string           `json:"acceptance,omitempty"` // accepted, rejected, local_rejected, unknown
	RequestedReasoning  ReasoningRequest `json:"requested_reasoning"`
	SentParameters      map[string]any   `json:"sent_parameters,omitempty"`
	SentParametersKnown bool             `json:"sent_parameters_known"`

	ReasoningTokensKnown bool              `json:"reasoning_tokens_known"`
	ReasoningTokens      uint64            `json:"reasoning_tokens,omitempty"`
	ReasoningContent     bool              `json:"reasoning_content,omitempty"`
	FinishReason         loom.FinishReason `json:"finish_reason,omitempty"`
	ResponsePreview      string            `json:"response_preview,omitempty"`
	Error                string            `json:"error,omitempty"`
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
// In schema v2 AcceptedReasoningEfforts records accepted requests, independently
// of reasoning evidence. Schema v1 also required observable reasoning.
// Neither version establishes independently implemented native budgets.
type ObservedCapabilities struct {
	ReasoningObservedEfforts []loom.ReasoningEffort `json:"reasoning_observed_efforts,omitempty"`
	Reasoning                loom.ReasoningSupport  `json:"reasoning"`
	AcceptedReasoningEfforts []loom.ReasoningEffort `json:"accepted_reasoning_efforts"`
	// StructuredOutput is the strongest observed success, not an exhaustive
	// profile when Coverage.StructuredOutput is false. Consult individual Checks.
	StructuredOutput loom.StructuredOutputMode `json:"structured_output"`
}

// Report is a serializable behavioral capability profile. Probe can return a
// partial Report alongside an error; completed checks remain valid evidence.
type Report struct {
	EffortCoverage *EffortCoverage      `json:"effort_coverage,omitempty"`
	SchemaVersion  int                  `json:"schema_version"`
	Model          string               `json:"model,omitempty"`
	Observed       ObservedCapabilities `json:"observed"`
	Coverage       Coverage             `json:"coverage"`
	Checks         []Check              `json:"checks"`
}

// Options controls probing. Calls are deliberately sequential to reduce rate
// limit pressure and make provider-side behavior easier to audit.
type Options struct {
	PerCallTimeout time.Duration
	// DeclaredCapabilities supplies administrator-supplied effort declarations.
	// It never changes the synthetic models or fills in business defaults.
	DeclaredCapabilities *loom.ModelCapabilities
	// DeclarationSource is an optional provenance label for that snapshot.
	DeclarationSource     string
	DeclarationSourceURLs []string
	// ReasoningEfforts nil uses DeclaredCapabilities.
	// There is no global candidate list. A non-nil empty slice skips efforts.
	// Explicit candidates may include aliases for diagnostic experiments.
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

// EffortCoverage records the exact universe and scope of this audit. A nil value
// on a historical report means coverage was not recorded; do not upgrade it.
// CandidateCoverageComplete means all supplied/declaration candidates got
// accepted/rejected server responses. Complete means all configured candidates
// were tested; it does not certify native model semantics.
// Neither flag proves independent native implementations.
type EffortCoverage struct {
	CandidateCoverageComplete bool                   `json:"candidate_coverage_complete"`
	UniverseKnown             bool                   `json:"universe_known"`
	CandidateSource           string                 `json:"candidate_source"`
	Source                    string                 `json:"source"`
	SourceURLs                []string               `json:"source_urls,omitempty"`
	Declared                  []loom.ReasoningEffort `json:"declared"`
	Candidates                []loom.ReasoningEffort `json:"candidates"`
	Tested                    []loom.ReasoningEffort `json:"tested"`
	Untested                  []loom.ReasoningEffort `json:"untested"`
	Unresolved                []loom.ReasoningEffort `json:"unresolved"`
	Complete                  bool                   `json:"complete"`
	NativeIndependenceProven  bool                   `json:"native_independence_proven"`
	Limitations               []string               `json:"limitations"`
}

// ReasoningRequest is the stable report representation of a requested switch
// and optional raw effort; SentParameters records actual adapter serialization.
type ReasoningRequest struct {
	Mode   loom.ReasoningMode   `json:"mode"`
	Effort loom.ReasoningEffort `json:"effort,omitempty"`
}
