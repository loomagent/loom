package modelprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/loomagent/loom"
)

const (
	defaultPerCallTimeout = 90 * time.Second
	maxPreviewRunes       = 240
)

var defaultEfforts = []loom.ReasoningEffort{
	loom.ReasoningEffortLow,
	loom.ReasoningEffortMedium,
	loom.ReasoningEffortHigh,
	loom.ReasoningEffortMax,
}

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
	efforts := options.ReasoningEfforts
	if efforts == nil {
		efforts = append([]loom.ReasoningEffort(nil), defaultEfforts...)
	}
	if err := validateEfforts(efforts); err != nil {
		return Report{}, err
	}

	omitModel, err := builder.Build(ctx, loom.ModelCapabilities{Reasoning: loom.ReasoningSupportNone})
	if err != nil {
		return Report{}, fmt.Errorf("modelprobe: build omit-parameter model: %w", err)
	}
	if omitModel == nil {
		return Report{}, errors.New("modelprobe: Builder returned a nil omit-parameter model")
	}
	rawModel, err := builder.Build(ctx, loom.ModelCapabilities{})
	if err != nil {
		return Report{}, fmt.Errorf("modelprobe: build passthrough model: %w", err)
	}
	if rawModel == nil {
		return Report{}, errors.New("modelprobe: Builder returned a nil passthrough model")
	}
	if omitModel.Name() != rawModel.Name() {
		return Report{}, fmt.Errorf("modelprobe: Builder returned different models %q and %q", omitModel.Name(), rawModel.Name())
	}

	report := Report{SchemaVersion: 1, Model: rawModel.Name(), Checks: []Check{}}
	defaultCheck := probeReasoning(ctx, omitModel, timeout, CheckReasoningDefault, loom.Reasoning{Mode: loom.ReasoningModeDisabled}, true, nil)
	disableCheck := probeReasoning(ctx, rawModel, timeout, CheckReasoningDisable, loom.Reasoning{Mode: loom.ReasoningModeDisabled}, false, options.ErrorClassifier)
	enableCheck := probeReasoning(ctx, rawModel, timeout, CheckReasoningEnable, loom.Reasoning{Mode: loom.ReasoningModeEnabled}, true, options.ErrorClassifier)
	report.Checks = append(report.Checks, defaultCheck, disableCheck, enableCheck)
	if err := ctx.Err(); err != nil {
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

	if enableCheck.Outcome == OutcomePositive {
		allEffortsConclusive := true
		for _, effort := range efforts {
			check := probeReasoning(ctx, rawModel, timeout, "reasoning.effort."+string(effort), loom.Reasoning{
				Mode: loom.ReasoningModeEnabled, Effort: effort,
			}, true, options.ErrorClassifier)
			check.Effort = effort
			report.Checks = append(report.Checks, check)
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if check.Outcome == OutcomeError {
				allEffortsConclusive = false
			} else if check.Outcome == OutcomePositive {
				report.Observed.AcceptedReasoningEfforts = append(report.Observed.AcceptedReasoningEfforts, effort)
			}
		}
		report.Coverage.AcceptedReasoningEfforts = allEffortsConclusive
	} else if enableCheck.Outcome == OutcomeNegative {
		// Efforts are conclusively empty when reasoning cannot be enabled.
		report.Coverage.AcceptedReasoningEfforts = true
	}

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
		return report, err
	}
	return report, nil
}

func probeReasoning(ctx context.Context, model loom.ChatModel, timeout time.Duration, name string, reasoning loom.Reasoning, positiveWhenReasoning bool, classify ErrorClassifier) Check {
	started := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := model.Chat(callCtx, loom.ChatRequest{
		Messages:  []loom.Message{{Role: loom.RoleUser, Content: "What is 1 + 1? Reply briefly."}},
		Reasoning: reasoning,
	})
	check := Check{Name: name, DurationMS: time.Since(started).Milliseconds()}
	if err != nil {
		check.Outcome = outcomeForError(err, classify)
		check.Evidence.Error = err.Error()
		return check
	}
	if response == nil {
		check.Outcome = OutcomeError
		check.Evidence.Error = "model returned a nil response"
		return check
	}
	check.Evidence.ReasoningTokens = response.Usage.ReasoningTokens
	check.Evidence.ReasoningContent = strings.TrimSpace(response.ReasoningContent) != ""
	check.Evidence.FinishReason = response.FinishReason
	check.Evidence.ResponsePreview = preview(response.Content)
	reasoningObserved := check.Evidence.ReasoningTokens > 0 || check.Evidence.ReasoningContent
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
	request.Reasoning = reasoning
	response, err := model.Chat(callCtx, request)
	check := Check{Name: name, DurationMS: time.Since(started).Milliseconds()}
	if err != nil {
		check.Outcome = outcomeForError(err, classify)
		check.Evidence.Error = err.Error()
		return check
	}
	if response == nil {
		check.Outcome = OutcomeError
		check.Evidence.Error = "model returned a nil response"
		return check
	}
	check.Evidence.FinishReason = response.FinishReason
	check.Evidence.ResponsePreview = preview(response.Content)
	var value any
	if err := json.Unmarshal([]byte(strings.TrimSpace(response.Content)), &value); err != nil {
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
	trueValue := any(true)
	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           map[string]*jsonschema.Schema{"ok": {Type: "boolean", Const: &trueValue}},
		Required:             []string{"ok"},
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
}

// DeriveReasoningSupport maps completed default, enable, and disable
// observations to Loom's four-state reasoning model.
func DeriveReasoningSupport(defaultOn, canEnable, canDisable bool) loom.ReasoningSupport {
	switch {
	case canEnable && canDisable:
		if defaultOn {
			return loom.ReasoningSupportToggleableDefaultOn
		}
		return loom.ReasoningSupportToggleableDefaultOff
	case canEnable && !canDisable:
		return loom.ReasoningSupportAlwaysOn
	case !canEnable && canDisable:
		if defaultOn {
			return loom.ReasoningSupportToggleableDefaultOn
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
		switch effort {
		case loom.ReasoningEffortLow, loom.ReasoningEffortMedium, loom.ReasoningEffortHigh, loom.ReasoningEffortMax:
		default:
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
