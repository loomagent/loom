package loom

import (
	"reflect"
	"testing"
)

func TestModelCapabilitiesClonePreservesDeclaration(t *testing.T) {
	for _, test := range []struct {
		name string
		caps ModelCapabilities
	}{
		{"unknown", ModelCapabilities{}},
		{"explicit empty efforts", ModelCapabilities{ReasoningEfforts: []ReasoningEffort{}}},
		{"complete declaration", ModelCapabilities{
			Reasoning: ReasoningSupportToggleable, ReasoningEfforts: []ReasoningEffort{"high", "low"},
			ReasoningEffortsUnconfirmed: true, OfficialDefaultReasoningEffort: "high",
			StructuredOutput: StructuredOutputJSONSchema, MaxContextTokens: 131072, MaxOutputTokens: 8192,
		}},
		{"diagnostic omit", ReasoningProbeCapabilities(true)},
		{"diagnostic passthrough", ReasoningProbeCapabilities(false)},
	} {
		t.Run(test.name, func(t *testing.T) {
			cloned := test.caps.Clone()
			if !reflect.DeepEqual(test.caps, cloned) {
				t.Fatalf("clone changed declaration: got %+v, want %+v", cloned, test.caps)
			}
			if len(cloned.ReasoningEfforts) > 0 {
				cloned.ReasoningEfforts[0] = "max"
				if test.caps.ReasoningEfforts[0] != "high" {
					t.Fatal("clone mutation changed the original declaration")
				}
				test.caps.ReasoningEfforts[1] = "xhigh"
				if cloned.ReasoningEfforts[1] != "low" {
					t.Fatal("original mutation changed the clone")
				}
			}
		})
	}
}

func TestModelCapabilitiesClonePreservesUndeclaredEffortChecks(t *testing.T) {
	for _, test := range []struct {
		name    string
		efforts []ReasoningEffort
		wantErr bool
	}{
		{"nil passes through", nil, false},
		{"empty rejects unlisted effort", []ReasoningEffort{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			caps := ModelCapabilities{ReasoningEfforts: test.efforts}.Clone()
			_, err := ResolveReasoning(caps, Reasoning{Mode: ReasoningModeEnabled, Effort: "custom"})
			if (err != nil) != test.wantErr {
				t.Fatalf("nil/empty effort semantics changed: %v", err)
			}
		})
	}
}
