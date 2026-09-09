package modelprobe

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/zhipuai"
)

const (
	defaultPerCallTimeout = 90 * time.Second
	maxPreviewRunes       = 240
)

// Probe runs a behavioral capability audit. Builder is called twice: once
// with reasoning declared unsupported so a disabled request omits the provider
// parameter, and once with undeclared capabilities so explicit parameters pass
// through without local capability gating.
func Probe(ctx context.Context, builder Builder, options Options) (Report, error) {
	if builder == nil {
		return Report{}, errors.New("modelprobe: Builder is required")
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	timeout := options.PerCallTimeout
	if timeout <= 0 {
		timeout = defaultPerCallTimeout
	}

	omitModel, err := builder.Build(ctx, loom.ReasoningProbeCapabilities(true))
	if err != nil {
		return Report{}, fmt.Errorf("modelprobe: build omit-parameter model: %w", err)
	}
	if omitModel == nil {
		return Report{}, errors.New("modelprobe: Builder returned a nil omit-parameter model")
	}
	rawModel, err := builder.Build(ctx, loom.ReasoningProbeCapabilities(false))
	if err != nil {
		return Report{}, fmt.Errorf("modelprobe: build passthrough model: %w", err)
	}
	if rawModel == nil {
		return Report{}, errors.New("modelprobe: Builder returned a nil passthrough model")
	}
	if omitModel.Name() != rawModel.Name() {
		return Report{}, fmt.Errorf("modelprobe: Builder returned different models %q and %q", omitModel.Name(), rawModel.Name())
	}

	coverage := effortCoverage(rawModel.Name(), options)
	efforts := coverage.Candidates
	if err := validateEfforts(efforts); err != nil {
		return Report{}, err
	}
	report := Report{SchemaVersion: 2, Model: rawModel.Name(), Checks: []Check{}, EffortCoverage: coverage}

	defaultCheck := probeReasoning(ctx, omitModel, timeout, CheckReasoningDefault, loom.Reasoning{Mode: loom.ReasoningModeDisabled}, true, nil)
	disableCheck := probeReasoning(ctx, rawModel, timeout, CheckReasoningDisable, loom.Reasoning{Mode: loom.ReasoningModeDisabled}, false, options.ErrorClassifier)
	enableCheck := probeReasoning(ctx, rawModel, timeout, CheckReasoningEnable, loom.Reasoning{Mode: loom.ReasoningModeEnabled}, true, options.ErrorClassifier)
	report.Checks = append(report.Checks, defaultCheck, disableCheck, enableCheck)
	if err := ctx.Err(); err != nil {
		finalizeEffortCoverage(&report)
		return report, err
	}

	if allConclusive(defaultCheck, disableCheck, enableCheck) {
		report.Coverage.ReasoningSupport = true
		report.Observed.Reasoning = DeriveReasoningSupport(
			defaultCheck.Outcome == OutcomePositive,
			enableCheck.Outcome == OutcomePositive,
			disableCheck.Outcome == OutcomePositive,
		)
	}

	// Effort requests are independent experiments: enabled-without-effort may
	// fail even when explicitly selected efforts work.
	for _, effort := range efforts {
		if err := ctx.Err(); err != nil {
			finalizeEffortCoverage(&report)
			return report, err
		}
		check := probeReasoning(ctx, rawModel, timeout, "reasoning.effort."+string(effort), loom.Reasoning{
			Mode: loom.ReasoningModeEnabled, Effort: effort,
		}, true, options.ErrorClassifier)
		check.Effort = effort
		report.Checks = append(report.Checks, check)
		coverage.Tested = append(coverage.Tested, effort)
		if check.Evidence.Acceptance == "accepted" {
			report.Observed.AcceptedReasoningEfforts = append(report.Observed.AcceptedReasoningEfforts, effort)
		}
		if check.Outcome == OutcomePositive {
			report.Observed.ReasoningObservedEfforts = append(report.Observed.ReasoningObservedEfforts, effort)
		}
		if check.Evidence.Acceptance != "accepted" && check.Evidence.Acceptance != "rejected" {
			coverage.Unresolved = append(coverage.Unresolved, effort)
		}
	}
	finalizeEffortCoverage(&report)

	structuredModel := omitModel
	structuredReasoning := loom.Reasoning{Mode: loom.ReasoningModeDisabled}
	if disableCheck.Outcome == OutcomePositive {
		structuredModel = rawModel
	} else if enableCheck.Outcome == OutcomePositive {
		structuredModel = rawModel
		structuredReasoning.Mode = loom.ReasoningModeEnabled
	}
	objectCheck, schemaCheck, err := probeStructuredOutput(ctx, structuredModel, timeout, structuredReasoning, options.ErrorClassifier)
	if err != nil {
		return Report{}, err
	}
	report.Checks = append(report.Checks, objectCheck, schemaCheck)
	if allConclusive(objectCheck, schemaCheck) || schemaCheck.Outcome == OutcomePositive {
		report.Coverage.StructuredOutput = true
		report.Observed.StructuredOutput = DeriveStructuredOutput(
			objectCheck.Outcome == OutcomePositive,
			schemaCheck.Outcome == OutcomePositive,
		)
	}
	if err := ctx.Err(); err != nil {
		finalizeEffortCoverage(&report)
		return report, err
	}
	return report, nil
}

func probeReasoning(ctx context.Context, model loom.ChatModel, timeout time.Duration, name string, reasoning loom.Reasoning, positiveWhenReasoning bool, classify ErrorClassifier) Check {
	started := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	evidence := requestEvidence(model, reasoning)
	response, err := model.Chat(callCtx, loom.ChatRequest{
		Messages:  []loom.Message{{Role: loom.RoleUser, Content: "Three boxes are labeled apples, oranges, and mixed; every label is wrong. Explain how drawing one fruit from one box identifies all three boxes."}},
		Reasoning: reasoning,
	})
	check := Check{Name: name, DurationMS: time.Since(started).Milliseconds(), Evidence: evidence}
	if err != nil {
		check.Outcome = outcomeForCheckError(name, err, classify)
		check.Evidence.Error = err.Error()
		check.Evidence.Acceptance = errorAcceptance(err, check.Outcome)
		return check
	}
	if response == nil {
		check.Outcome = OutcomeError
		check.Evidence.Error = "model returned a nil response"
		return check
	}
	check.Evidence.Acceptance = "accepted"
	check.Evidence.ReasoningTokensKnown = response.Usage.ReasoningTokensKnown
	check.Evidence.ReasoningTokens = response.Usage.ReasoningTokens
	check.Evidence.ReasoningContent = strings.TrimSpace(response.ReasoningContent) != ""
	check.Evidence.FinishReason = response.FinishReason
	check.Evidence.ResponsePreview = preview(response.Content)
	reasoningObserved := check.Evidence.ReasoningTokens > 0 || check.Evidence.ReasoningContent
	// Preserve the stricter GLM audit: a zero-token response from this
	// always-on provider is not sufficient evidence of switch-off semantics.
	if strings.HasPrefix(model.Name(), "zhipuai/") && (response.FinishReason != loom.FinishReasonStop || !reasoningObserved) {
		check.Outcome = OutcomeError
		check.Evidence.Error = "insufficient evidence: no positive reasoning signal or response did not finish normally"
		return check
	}
	if !reasoningObserved && (!response.Usage.ReasoningTokensKnown || response.FinishReason != loom.FinishReasonStop) {
		check.Outcome = OutcomeError
		check.Evidence.Error = "insufficient evidence: reasoning telemetry absent or response incomplete"
		return check
	}

	if reasoningObserved == positiveWhenReasoning {
		check.Outcome = OutcomePositive
	} else {
		check.Outcome = OutcomeNegative
	}
	return check
}

func probeStructuredOutput(ctx context.Context, model loom.ChatModel, timeout time.Duration, reasoning loom.Reasoning, classify ErrorClassifier) (Check, Check, error) {
	schema := probeSchema()
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return Check{}, Check{}, fmt.Errorf("modelprobe: resolve probe schema: %w", err)
	}
	object := probeStructured(ctx, model, timeout, CheckStructuredJSONObject, reasoning,
		loom.ChatRequest{ResponseFormat: loom.ResponseFormatJSONObject}, nil, classify)
	schemaCheck := probeStructured(ctx, model, timeout, CheckStructuredJSONSchema, reasoning,
		loom.ChatRequest{StructuredOutput: &loom.StructuredOutput{
			Mode: loom.StructuredOutputJSONSchema, Name: "loom_model_probe", Schema: schema,
		}}, resolved, classify)
	return object, schemaCheck, nil
}

func probeStructured(ctx context.Context, model loom.ChatModel, timeout time.Duration, name string, reasoning loom.Reasoning, request loom.ChatRequest, schema *jsonschema.Resolved, classify ErrorClassifier) Check {
	started := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request.Messages = []loom.Message{{Role: loom.RoleUser, Content: `Return exactly one JSON object with this shape: {"ok": true}`}}
	if schema != nil {
		request.Messages[0].Content = "Return an object conforming to the supplied response schema."
	}
	request.Reasoning = reasoning
	evidence := requestEvidence(model, reasoning)
	response, err := model.Chat(callCtx, request)
	check := Check{Name: name, DurationMS: time.Since(started).Milliseconds(), Evidence: evidence}
	if err != nil {
		check.Outcome = outcomeForCheckError(name, err, classify)
		check.Evidence.Error = err.Error()
		check.Evidence.Acceptance = errorAcceptance(err, check.Outcome)
		return check
	}
	if response == nil {
		check.Outcome = OutcomeError
		check.Evidence.Error = "model returned a nil response"
		return check
	}
	check.Evidence.Acceptance = "accepted"
	check.Evidence.FinishReason = response.FinishReason
	check.Evidence.ResponsePreview = preview(response.Content)
	var value any
	if err := jsonv2.Unmarshal([]byte(strings.TrimSpace(response.Content)), &value); err != nil {
		check.Outcome = OutcomeNegative
		check.Evidence.Error = "response is not valid JSON: " + err.Error()
		return check
	}
	if _, ok := value.(map[string]any); !ok {
		check.Outcome = OutcomeNegative
		check.Evidence.Error = "response is valid JSON but not an object"
		return check
	}
	if schema != nil {
		if err := schema.Validate(value); err != nil {
			check.Outcome = OutcomeNegative
			check.Evidence.Error = "response does not satisfy schema: " + err.Error()
			return check
		}
	}
	check.Outcome = OutcomePositive
	return check
}

func probeSchema() *jsonschema.Schema {
	type response struct {
		OK bool `json:"ok" jsonschema:"Whether the probe succeeded. Must be true."`
	}
	schema := loom.MustSchemaFor[response]()
	trueValue := any(true)
	schema.Properties["ok"].Const = &trueValue
	return schema
}

// DeriveReasoningSupport maps completed default, enable, and disable
// observations to Loom's three-state reasoning model.
func DeriveReasoningSupport(defaultOn, canEnable, canDisable bool) loom.ReasoningSupport {
	switch {
	case canEnable && canDisable:
		return loom.ReasoningSupportToggleable
	case canEnable && !canDisable:
		return loom.ReasoningSupportAlwaysOn
	case !canEnable && canDisable:
		if defaultOn {
			return loom.ReasoningSupportToggleable
		}
		return loom.ReasoningSupportNone
	default:
		if defaultOn {
			return loom.ReasoningSupportAlwaysOn
		}
		return loom.ReasoningSupportNone
	}
}

// DeriveStructuredOutput maps completed behavior checks to the strongest
// supported structured-output mode.
func DeriveStructuredOutput(object, schema bool) loom.StructuredOutputMode {
	if schema {
		return loom.StructuredOutputJSONSchema
	}
	if object {
		return loom.StructuredOutputJSONObject
	}
	return loom.StructuredOutputNone
}

func validateEfforts(efforts []loom.ReasoningEffort) error {
	seen := map[loom.ReasoningEffort]struct{}{}
	for _, effort := range efforts {
		if !loom.ValidReasoningEffort(effort) {
			return fmt.Errorf("modelprobe: invalid reasoning effort %q", effort)
		}

		if _, ok := seen[effort]; ok {
			return fmt.Errorf("modelprobe: duplicate reasoning effort %q", effort)
		}
		seen[effort] = struct{}{}
	}
	return nil
}

func allConclusive(checks ...Check) bool {
	for _, check := range checks {
		if check.Outcome == OutcomeError {
			return false
		}
	}
	return true
}

func outcomeForError(err error, classify ErrorClassifier) Outcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return OutcomeError
	}
	if classify != nil && classify(err) == ErrorUnsupported {
		return OutcomeNegative
	}
	return OutcomeError
}

func preview(content string) string {
	content = strings.TrimSpace(content)
	runes := []rune(content)
	if len(runes) > maxPreviewRunes {
		return string(runes[:maxPreviewRunes])
	}
	return content
}

// A provider rejection must identify the tested field; billing, authentication,
// availability and unrelated invalid parameters are inconclusive.
func outcomeForCheckError(name string, err error, classify ErrorClassifier) Outcome {
	var local *loom.RequestValidationError
	if errors.As(err, &local) {
		return OutcomeError
	}
	if name == CheckReasoningDefault {
		return OutcomeError
	}
	var apiErr *zhipuai.APIError
	if errors.As(err, &apiErr) {
		field := "thinking"
		if strings.HasPrefix(name, "reasoning.effort.") {
			field = "reasoning_effort"
		}
		if strings.HasPrefix(name, "structured_output.") {
			field = "response_format"
		}
		if apiErr.RejectsCapability(field) {
			return OutcomeNegative
		}
		return OutcomeError
	}
	return outcomeForError(err, classify)
}

func effortCoverage(name string, options Options) *EffortCoverage {
	c := &EffortCoverage{Source: "unknown", Limitations: []string{"Request acceptance and returned reasoning do not prove independent native efforts."}}
	if options.DeclaredCapabilities != nil {
		c.UniverseKnown = options.DeclaredCapabilities.Reasoning != "" || options.DeclaredCapabilities.ReasoningEfforts != nil
		c.Source = "model_declaration"
		if options.DeclarationSource != "" {
			c.Source = options.DeclarationSource
		}
		c.SourceURLs = slices.Clone(options.DeclarationSourceURLs)
		c.Declared = slices.Clone(options.DeclaredCapabilities.ReasoningEfforts)
		c.Limitations = append(c.Limitations, "Only administrator-supplied efforts are tested; successful calls do not certify native semantics.")
	} else {
		c.Limitations = append(c.Limitations, "No explicit effort declaration; no effort candidates will be guessed.")
	}
	c.CandidateSource = c.Source
	c.Candidates = slices.Clone(c.Declared)
	if options.ReasoningEfforts != nil {
		c.CandidateSource = "explicit_candidates"
		c.Candidates = slices.Clone(options.ReasoningEfforts)
	}
	return c
}

func finalizeEffortCoverage(report *Report) {
	c := report.EffortCoverage
	if c == nil {
		return
	}
	c.Untested = nil
	for _, e := range append(slices.Clone(c.Declared), c.Candidates...) {
		if !slices.Contains(c.Tested, e) && !slices.Contains(c.Untested, e) {
			c.Untested = append(c.Untested, e)
		}
	}
	c.CandidateCoverageComplete = c.UniverseKnown && len(c.Untested) == 0 && len(c.Unresolved) == 0
	c.Complete = c.CandidateCoverageComplete
	report.Coverage.AcceptedReasoningEfforts = c.CandidateCoverageComplete
}

// Built-in adapters expose their serialized parameters. Custom models without
// this optional interface retain unknown wire evidence rather than a guess.
func requestEvidence(model loom.ChatModel, r loom.Reasoning) Evidence {
	e := Evidence{RequestedReasoning: ReasoningRequest{Mode: r.Mode, Effort: r.Effort}, Acceptance: "unknown"}
	if inspector, ok := model.(interface {
		ReasoningRequestParameters(loom.Reasoning) (map[string]any, error)
	}); ok {
		if parameters, err := inspector.ReasoningRequestParameters(r); err == nil {
			e.SentParameters = parameters
			e.SentParametersKnown = true
		}
	}
	return e
}

func errorAcceptance(err error, outcome Outcome) string {
	var local *loom.RequestValidationError
	if errors.As(err, &local) {
		return "local_rejected"
	}
	if outcome == OutcomeNegative {
		return "rejected"
	}
	return "unknown"
}
