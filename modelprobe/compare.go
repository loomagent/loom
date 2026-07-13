package modelprobe

import (
	"slices"
	"strings"

	"github.com/loomagent/loom"
)

// Compare returns confirmed differences between a declared capability profile
// and a probe report. Fields without coverage are intentionally ignored.
func Compare(declared loom.ModelCapabilities, report Report) []Mismatch {
	var mismatches []Mismatch
	if report.Coverage.ReasoningSupport && declared.Reasoning != report.Observed.Reasoning {
		mismatches = append(mismatches, Mismatch{Field: "reasoning_support", Declared: string(declared.Reasoning), Observed: string(report.Observed.Reasoning)})
	}
	if report.Coverage.AcceptedReasoningEfforts && report.Observed.Reasoning != loom.ReasoningSupportNone &&
		!sameEffortSet(declared.ReasoningEfforts, report.Observed.AcceptedReasoningEfforts) {
		mismatches = append(mismatches, Mismatch{Field: "reasoning_efforts", Declared: joinEfforts(declared.ReasoningEfforts), Observed: joinEfforts(report.Observed.AcceptedReasoningEfforts)})
	}
	if report.Coverage.StructuredOutput && declared.StructuredOutput != report.Observed.StructuredOutput {
		mismatches = append(mismatches, Mismatch{Field: "structured_output", Declared: string(declared.StructuredOutput), Observed: string(report.Observed.StructuredOutput)})
	}
	return mismatches
}

func sameEffortSet(a, b []loom.ReasoningEffort) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func joinEfforts(efforts []loom.ReasoningEffort) string {
	if len(efforts) == 0 {
		return "(empty)"
	}
	values := make([]string, len(efforts))
	for i, effort := range efforts {
		values[i] = string(effort)
	}
	slices.Sort(values)
	return strings.Join(values, ",")
}
