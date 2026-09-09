package modelprobe

import (
	"testing"

	"github.com/loomagent/loom"
)

func TestCompareConfirmedFields(t *testing.T) {
	report := Report{
		Observed: ObservedCapabilities{
			Reasoning:                loom.ReasoningSupportToggleableDefaultOff,
			AcceptedReasoningEfforts: []loom.ReasoningEffort{loom.ReasoningEffortHigh, loom.ReasoningEffortLow},
			StructuredOutput:         loom.StructuredOutputJSONSchema,
		},
		Coverage: Coverage{ReasoningSupport: true, AcceptedReasoningEfforts: true, StructuredOutput: true},
	}
	declared := loom.ModelCapabilities{
		Reasoning:        loom.ReasoningSupportAlwaysOn,
		ReasoningEfforts: []loom.ReasoningEffort{loom.ReasoningEffortLow, loom.ReasoningEffortHigh},
		StructuredOutput: loom.StructuredOutputJSONObject,
	}
	got := Compare(declared, report)
	if len(got) != 2 || got[0].Field != "reasoning_support" || got[1].Field != "structured_output" {
		t.Fatalf("mismatches = %+v", got)
	}
}

func TestCompareDoesNotUpgradeHistoricalEffortCoverage(t *testing.T) {
	r := Report{SchemaVersion: 1, Coverage: Coverage{AcceptedReasoningEfforts: true}, Observed: ObservedCapabilities{AcceptedReasoningEfforts: []loom.ReasoningEffort{"low", "medium", "high", "max"}}}
	declared := loom.ModelCapabilities{ReasoningEfforts: []loom.ReasoningEffort{"minimal", "xhigh"}}
	if got := Compare(declared, r); len(got) != 0 {
		t.Fatalf("legacy complete assumption: %+v", got)
	}
}
