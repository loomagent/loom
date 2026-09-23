package react

import (
	"context"
	"errors"
	"fmt"

	"github.com/loomagent/loom"
)

// ErrInvalidToolExecution means a host scheduler broke the result identity contract.
var ErrInvalidToolExecution = errors.New("invalid tool executor results")

// executeStepTools reserves in model order before handing an eligible batch to
// a scheduler. Rejected calls keep their original position in model history.
func executeStepTools(ctx context.Context, w loom.Writer, state State, cfg Config, visible []*loom.ToolInfo, calls []loom.ToolCall, total *uint64, uses map[string]uint64, exempt map[string]bool) ([]loom.ToolExecResult, bool, string, error) {
	names := make([]string, 0, len(visible))
	for _, info := range visible {
		if info != nil {
			names = append(names, info.Name)
		}
	}
	registry, err := cfg.Tools.Subset(names)
	if err != nil {
		return nil, false, "", err
	}
	results := make([]loom.ToolExecResult, len(calls))
	pending := make([]loom.ToolCall, 0, len(calls))
	positions := make([]int, 0, len(calls))
	refusal := ""
	for i, call := range calls {
		code, reason := "", ""
		if why := toolBudgetReason(call.Name, cfg, *total, uses, exempt); why != "" {
			code, reason = "tool_budget_exhausted", why
		} else if _, ok := registry.Lookup(call.Name); !ok {
			code, reason = "tool_not_available", fmt.Sprintf("tool %q not registered for this step", call.Name)
		} else if why := reserveTool(call.Name, cfg, total, uses, exempt); why != "" {
			code, reason = "tool_budget_exhausted", why
		}
		if reason != "" {
			refusal = code
			result, err := rejectToolCall(ctx, w, call, code, reason)
			if err != nil {
				return nil, false, refusal, err
			}
			results[i] = result
			continue
		}
		pending = append(pending, call)
		positions = append(positions, i)
	}
	if len(pending) == 0 {
		return results, false, refusal, nil
	}
	var done []loom.ToolExecResult
	if cfg.ExecuteTools != nil {
		// Keep an immutable copy for contract validation after the host returns.
		done, err = cfg.ExecuteTools(ctx, w, state, registry, append([]loom.ToolCall(nil), pending...))
	} else {
		done, err = loom.ExecuteToolCalls(ctx, w, registry, pending)
	}
	if err != nil {
		return nil, false, refusal, err
	}
	if len(done) != len(pending) {
		return nil, false, refusal, fmt.Errorf("%w: got %d results for %d calls", ErrInvalidToolExecution, len(done), len(pending))
	}
	for i, result := range done {
		if result.Call != pending[i] {
			return nil, false, refusal, fmt.Errorf("%w: changed call identity at index %d", ErrInvalidToolExecution, i)
		}
		results[positions[i]] = result
	}
	return results, true, refusal, nil
}
