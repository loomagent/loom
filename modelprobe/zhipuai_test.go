package modelprobe

import (
	"context"
	"testing"
	"time"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/zhipuai"
)

type zhipuProbeModel struct{ *fakeModel }

func (*zhipuProbeModel) Name() string { return "zhipuai/glm-5.3" }

func TestZhipuProbeDoesNotInferDisabledFromMissingTelemetry(t *testing.T) {
	for _, known := range []bool{false, true} {
		model := &zhipuProbeModel{&fakeModel{handler: func(loom.ModelCapabilities, loom.ChatRequest) (*loom.ChatResponse, error) {
			return &loom.ChatResponse{Content: "answer", FinishReason: loom.FinishReasonStop, Usage: loom.Usage{ReasoningTokensKnown: known}}, nil
		}}}
		check := probeReasoning(context.Background(), model, time.Second, CheckReasoningDisable, loom.Reasoning{Mode: loom.ReasoningModeDisabled}, false, nil)
		if check.Outcome != OutcomeError {
			t.Fatalf("known=%t: %+v", known, check)
		}
	}
}
func TestZhipuProbeRejectsOnlyTestedCapability(t *testing.T) {
	unsupported := func(error) ErrorDisposition { return ErrorUnsupported }
	for _, test := range []struct {
		name string
		err  *zhipuai.APIError
		want Outcome
	}{
		{CheckReasoningDisable, &zhipuai.APIError{StatusCode: 400, Code: "1210", Message: "thinking is unsupported"}, OutcomeNegative},
		{CheckReasoningDefault, &zhipuai.APIError{StatusCode: 400, Code: "1210", Message: "thinking is unsupported"}, OutcomeError},
		{CheckReasoningDisable, &zhipuai.APIError{StatusCode: 429, Code: "1113", Message: "insufficient balance"}, OutcomeError},
		{CheckReasoningDisable, &zhipuai.APIError{StatusCode: 400, Code: "1210", Message: "temperature is unsupported"}, OutcomeError},
	} {
		if got := outcomeForCheckError(test.name, test.err, unsupported); got != test.want {
			t.Fatalf("%+v: %s", test, got)
		}
	}
}
func TestCompareLegacyToggleable(t *testing.T) {
	report := Report{Coverage: Coverage{ReasoningSupport: true}, Observed: ObservedCapabilities{Reasoning: loom.ReasoningSupportToggleable}}
	for _, legacy := range []loom.ReasoningSupport{loom.ReasoningSupportToggleableDefaultOn, loom.ReasoningSupportToggleableDefaultOff} {
		if got := Compare(loom.ModelCapabilities{Reasoning: legacy}, report); len(got) != 0 {
			t.Fatalf("%s: %+v", legacy, got)
		}
	}
}
