package loom

import "testing"

func TestExactManualReasoningDeclaration(t *testing.T) {
	caps := ModelCapabilities{Reasoning: ReasoningSupportToggleable, ReasoningEfforts: []ReasoningEffort{"low", "high", "max", "Exact-Level"}}
	for _, provider := range []string{"ark", "deepseek", "openrouter", "zhipuai"} {
		for _, effort := range caps.ReasoningEfforts {
			got, err := ResolveModelReasoning(provider, "any-model", caps, Reasoning{Mode: ReasoningModeEnabled, Effort: effort})
			if err != nil || got.Effort != effort {
				t.Fatalf("%s %s: %+v %v", provider, effort, got, err)
			}
		}
		for _, r := range []Reasoning{{Mode: ReasoningModeEnabled, Effort: "medium"}, {Mode: ReasoningModeEnabled, Effort: "exact-level"}, {Mode: ReasoningModeEnabled}, {Mode: ReasoningModeDisabled, Effort: "high"}, {Effort: "high"}} {
			if _, err := ResolveModelReasoning(provider, "any-model", caps, r); err == nil {
				t.Fatalf("accepted undeclared request %+v", r)
			}
		}
	}
	caps.ReasoningEffortsUnconfirmed = true
	if _, err := ResolveReasoning(caps, Reasoning{Mode: ReasoningModeEnabled, Effort: "high"}); err == nil {
		t.Fatal("unconfirmed declaration enabled reasoning")
	}
	if _, err := ResolveReasoning(caps, Reasoning{Mode: ReasoningModeDisabled}); err != nil {
		t.Fatal(err)
	}
}
func TestDeclarationValidationDoesNotInferModelCapabilities(t *testing.T) {
	for _, efforts := range [][]ReasoningEffort{{"high", "high"}, {""}, {" high"}, {"a/b"}} {
		if err := ValidateModelReasoningCapabilities("ark", "model", ModelCapabilities{Reasoning: ReasoningSupportToggleable, ReasoningEfforts: efforts}); err == nil {
			t.Fatalf("accepted %v", efforts)
		}
	}
	if err := ValidateModelReasoningCapabilities("ark", "doubao-seed-evolving", ModelCapabilities{Reasoning: ReasoningSupportToggleable, ReasoningEfforts: []ReasoningEffort{"Custom"}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateModelReasoningCapabilities("ark", "model", ModelCapabilities{Reasoning: ReasoningSupportNone, ReasoningEfforts: []ReasoningEffort{"high"}}); err == nil {
		t.Fatal("non-reasoning model has efforts")
	}
}
