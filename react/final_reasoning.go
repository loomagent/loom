// final_reasoning.go derives the lowest legal reasoning configuration for final delivery.
package react

import (
	"fmt"

	"github.com/loomagent/loom"
)

// finalReasoningEffortRank orders only the provider-native efforts that Loom
// knows how to compare. Custom efforts remain usable when the model declares
// exactly one value; Loom does not guess an order for multiple custom values.
var finalReasoningEffortRank = map[loom.ReasoningEffort]int{
	loom.ReasoningEffortMinimal: 0,
	loom.ReasoningEffortLow:     1,
	loom.ReasoningEffortMedium:  2,
	loom.ReasoningEffortHigh:    3,
	loom.ReasoningEffortXHigh:   4,
	loom.ReasoningEffortMax:     5,
}

// minimizeFinalReasoning chooses the least expensive legal reasoning setting
// for a user-visible final answer. Models without declared capabilities keep the
// explicitly configured value so direct Loom users are not affected.
func minimizeFinalReasoning(caps loom.ModelCapabilities, configured loom.Reasoning) (loom.Reasoning, error) {
	switch caps.Reasoning.Canonical() {
	case "":
		return configured, nil
	case loom.ReasoningSupportNone, loom.ReasoningSupportToggleable:
		return loom.Reasoning{Mode: loom.ReasoningModeDisabled}, nil
	case loom.ReasoningSupportAlwaysOn:
		if len(caps.ReasoningEfforts) == 0 {
			return loom.Reasoning{Mode: loom.ReasoningModeEnabled}, nil
		}
		if effort, ok := lowestKnownReasoningEffort(caps.ReasoningEfforts); ok {
			return loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: effort}, nil
		}
		if len(caps.ReasoningEfforts) == 1 {
			return loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: caps.ReasoningEfforts[0]}, nil
		}
		return loom.Reasoning{}, fmt.Errorf("cannot determine the lowest effort from %v", caps.ReasoningEfforts)
	default:
		return loom.Reasoning{}, fmt.Errorf("unknown reasoning support %q", caps.Reasoning)
	}
}

// lowestKnownReasoningEffort returns the minimum effort Loom can order without
// relying on provider declaration order.
func lowestKnownReasoningEffort(efforts []loom.ReasoningEffort) (loom.ReasoningEffort, bool) {
	var lowest loom.ReasoningEffort
	lowestRank := 0
	for _, effort := range efforts {
		rank, known := finalReasoningEffortRank[effort]
		if !known {
			continue
		}
		if lowest == "" || rank < lowestRank {
			lowest = effort
			lowestRank = rank
		}
	}
	return lowest, lowest != ""
}
