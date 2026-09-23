package react

import (
	"context"
	"errors"
	"testing"

	"github.com/loomagent/loom"
)

// A model that keeps bundling the terminal tool with others is making no progress, so the run stops
// with a clear error instead of asking again until an unlimited MaxSteps runs out.
func TestConsecutiveRefusedBatchesAreBounded(t *testing.T) {
	t.Parallel()
	terminalCalls, lookupCalls := 0, 0
	lookup := loom.NewArgsTool(loom.MustArgsContract("lookup"), "Look a term up.",
		func(context.Context, loom.Args) (string, error) { lookupCalls++; return `{}`, nil })
	mixed := &loom.ChatResponse{
		ToolCalls:    []loom.ToolCall{{ID: "c1", Name: "lookup"}, {ID: "c2", Name: "finalize_answer"}},
		FinishReason: loom.FinishReasonToolCalls,
	}
	model := &scriptedModel{responses: []*loom.ChatResponse{mixed, mixed, mixed, mixed}}
	cfg := Config{Model: model, Tools: loom.NewToolRegistry(lookup, terminalTool(&terminalCalls))}
	result, err := runTerminalCase(t, cfg, "refusal-bound")
	refused, ok := errors.AsType[*RefusedBatchesError](err)
	if !ok {
		t.Fatalf("err = %v, want a RefusedBatchesError", err)
	}
	if refused.Refusals != defaultMaxConsecutiveRefusals || refused.Reason != "terminal_tool_not_alone" {
		t.Fatalf("refusal = %+v", refused)
	}
	if result != nil {
		t.Fatalf("a stopped run must not report a result: %+v", result)
	}
	if lookupCalls != 0 || terminalCalls != 0 {
		t.Fatalf("nothing may run: lookup=%d terminal=%d", lookupCalls, terminalCalls)
	}
	if len(model.requests) != int(defaultMaxConsecutiveRefusals) {
		t.Fatalf("requests = %d, want %d: the run must stop before asking again", len(model.requests), defaultMaxConsecutiveRefusals)
	}
	// The refusals were written as tool results before the run stopped.
	if content := toolResultContent(model.requests[1].Messages, "c1"); content == "" {
		t.Fatal("the refusal must be recorded in the conversation")
	}
}

// One real execution is progress, so the counter starts over.
func TestRefusalCounterResetsAfterExecution(t *testing.T) {
	t.Parallel()
	terminalCalls, lookupCalls := 0, 0
	lookup := loom.NewArgsTool(loom.MustArgsContract("lookup"), "Look a term up.",
		func(context.Context, loom.Args) (string, error) { lookupCalls++; return `{}`, nil })
	mixed := &loom.ChatResponse{
		ToolCalls:    []loom.ToolCall{{ID: "c1", Name: "lookup"}, {ID: "c2", Name: "finalize_answer"}},
		FinishReason: loom.FinishReasonToolCalls,
	}
	lookupOnly := &loom.ChatResponse{
		ToolCalls:    []loom.ToolCall{{ID: "c3", Name: "lookup"}},
		FinishReason: loom.FinishReasonToolCalls,
	}
	model := &scriptedModel{responses: []*loom.ChatResponse{
		mixed, lookupOnly, mixed, lookupOnly,
		{ToolCalls: []loom.ToolCall{{ID: "c4", Name: "finalize_answer"}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "the answer", FinishReason: loom.FinishReasonStop},
	}}
	cfg := Config{Model: model, Tools: loom.NewToolRegistry(lookup, terminalTool(&terminalCalls))}
	result, err := runTerminalCase(t, cfg, "refusal-reset")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if lookupCalls != 2 || terminalCalls != 1 {
		t.Fatalf("lookup=%d terminal=%d, want 2 and 1", lookupCalls, terminalCalls)
	}
	if result == nil || result.EndedByTool != "finalize_answer" || !result.ToolPhaseEnded {
		t.Fatalf("result = %+v", result)
	}
}

// Budget refusals count too: switching the reason does not make a spinning model progress.
func TestBudgetRefusalsAlsoCount(t *testing.T) {
	t.Parallel()
	terminalCalls, lookupCalls := 0, 0
	lookup := loom.NewArgsTool(loom.MustArgsContract("lookup"), "Look a term up.",
		func(context.Context, loom.Args) (string, error) { lookupCalls++; return `{}`, nil })
	lookupOnly := &loom.ChatResponse{
		ToolCalls:    []loom.ToolCall{{ID: "c1", Name: "lookup"}},
		FinishReason: loom.FinishReasonToolCalls,
	}
	model := &scriptedModel{responses: []*loom.ChatResponse{lookupOnly, lookupOnly, lookupOnly, lookupOnly}}
	cfg := Config{Model: model, Tools: loom.NewToolRegistry(lookup, terminalTool(&terminalCalls)), MaxToolCalls: 1}
	_, err := runTerminalCase(t, cfg, "budget-refusal-bound")
	refused, ok := errors.AsType[*RefusedBatchesError](err)
	if !ok {
		t.Fatalf("err = %v, want a RefusedBatchesError", err)
	}
	if refused.Reason != "tool_budget_exhausted" {
		t.Fatalf("refusal = %+v", refused)
	}
	if lookupCalls != 1 {
		t.Fatalf("lookup ran %d times, want the one call the budget allowed", lookupCalls)
	}
}
