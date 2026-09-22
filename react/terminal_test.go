package react

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/loomagent/loom"
)

// runTerminalCase drives the loop with one marked tool and returns what it produced.
func runTerminalCase(t *testing.T, cfg Config, conversationID string) (*Result, error) {
	t.Helper()
	var result *Result
	var runErr error
	if _, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		result, runErr = Run(ctx, w, cfg)
		return runErr
	}, loom.RunOptions{ConversationID: conversationID}); err != nil && !errors.Is(err, ErrToolCallInFinalPhase) {
		t.Fatalf("loom.Run: %v", err)
	}
	return result, runErr
}

// toolResultContent returns what the model was told about one call, or an empty string.
func toolResultContent(messages []loom.Message, callID string) string {
	for _, message := range messages {
		if message.Role == loom.RoleTool && message.ToolCallID == callID {
			return message.Content
		}
	}
	return ""
}

func terminalTool(calls *int) loom.Tool {
	return loom.NewArgsTool(loom.MustArgsContract("finalize_answer"), "End the tool-using phase, on its own.",
		func(context.Context, loom.Args) (string, error) {
			*calls++
			return `{"ok":true}`, nil
		}, loom.WithEndsToolPhase())
}

// A successful call ends the phase, and every later request carries no tools, which is what makes
// that round's content the answer by construction.
func TestTerminalToolEndsTheToolPhase(t *testing.T) {
	t.Parallel()
	terminalCalls := 0
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "c1", Name: "finalize_answer"}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "the answer", FinishReason: loom.FinishReasonStop},
	}}
	cfg := Config{Model: model, Tools: loom.NewToolRegistry(terminalTool(&terminalCalls))}
	result, err := runTerminalCase(t, cfg, "terminal-phase")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if terminalCalls != 1 {
		t.Fatalf("terminal tool ran %d times, want 1", terminalCalls)
	}
	if result == nil || !result.ToolPhaseEnded || result.EndedByTool != "finalize_answer" || result.FinalContent != "the answer" {
		t.Fatalf("result = %+v", result)
	}
	if len(model.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(model.requests))
	}
	if len(model.requests[0].Tools) != 1 {
		t.Fatalf("the first request must offer the tool: %d", len(model.requests[0].Tools))
	}
	if len(model.requests[1].Tools) != 0 {
		t.Fatalf("the final phase must carry no tools, got %d", len(model.requests[1].Tools))
	}
	if len(model.requests[1].Messages) == 0 {
		t.Fatal("the final request must tell the model what changed")
	}
	last := model.requests[1].Messages[len(model.requests[1].Messages)-1]
	if last.Role != loom.RoleSystem || !strings.Contains(last.Content, "tool phase is over") {
		t.Fatalf("the final request must announce the phase change, got %+v", last)
	}
}

// A terminal tool called together with anything else is refused whole: nothing in the batch runs,
// every call still gets a model-facing result, and the phase stays open.
func TestMixedTerminalBatchIsRejectedWhole(t *testing.T) {
	t.Parallel()
	terminalCalls, lookupCalls := 0, 0
	lookup := loom.NewArgsTool(loom.MustArgsContract("lookup"), "Look a term up.",
		func(context.Context, loom.Args) (string, error) { lookupCalls++; return `{"found":true}`, nil })
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "c1", Name: "lookup"}, {ID: "c2", Name: "finalize_answer"}}, FinishReason: loom.FinishReasonToolCalls},
		{ToolCalls: []loom.ToolCall{{ID: "c3", Name: "finalize_answer"}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "the answer", FinishReason: loom.FinishReasonStop},
	}}
	cfg := Config{Model: model, Tools: loom.NewToolRegistry(lookup, terminalTool(&terminalCalls))}
	result, err := runTerminalCase(t, cfg, "mixed-batch")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if lookupCalls != 0 || terminalCalls != 1 {
		t.Fatalf("nothing in the refused batch may run: lookup=%d terminal=%d", lookupCalls, terminalCalls)
	}
	if len(model.requests) != 3 {
		t.Fatalf("requests = %d, want 3: the refused batch must not end the phase", len(model.requests))
	}
	if len(model.requests[1].Tools) != 2 {
		t.Fatalf("the phase must stay open after a refused batch, got %d tools", len(model.requests[1].Tools))
	}
	for _, callID := range []string{"c1", "c2"} {
		content := toolResultContent(model.requests[1].Messages, callID)
		if !strings.Contains(content, "must be called on its own") || !strings.Contains(content, "still open") {
			t.Fatalf("call %s got %q, want the refusal it can act on", callID, content)
		}
	}
	if result == nil || !result.ToolPhaseEnded || result.EndedByTool != "finalize_answer" {
		t.Fatalf("result = %+v", result)
	}
}

// The rule is a call count, so calling the terminal tool twice is refused as well.
func TestDuplicateTerminalCallsAreRejected(t *testing.T) {
	t.Parallel()
	terminalCalls := 0
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{
			{ID: "c1", Name: "finalize_answer"},
			{ID: "c2", Name: "finalize_answer"},
		}, FinishReason: loom.FinishReasonToolCalls},
		{ToolCalls: []loom.ToolCall{{ID: "c3", Name: "finalize_answer"}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "the answer", FinishReason: loom.FinishReasonStop},
	}}
	cfg := Config{Model: model, Tools: loom.NewToolRegistry(terminalTool(&terminalCalls))}
	if _, err := runTerminalCase(t, cfg, "duplicate-terminal"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if terminalCalls != 1 {
		t.Fatalf("terminal tool ran %d times, want 1: the duplicated batch must not run", terminalCalls)
	}
	for _, callID := range []string{"c1", "c2"} {
		if content := toolResultContent(model.requests[1].Messages, callID); !strings.Contains(content, "in one batch") {
			t.Fatalf("call %s got %q, want the duplicate refusal", callID, content)
		}
	}
}

// A final phase has no tools to call, so a tool call there is not executed, and the caller decides
// whether to ask for the answer again.
func TestToolCallInFinalPhaseIsNotExecuted(t *testing.T) {
	t.Parallel()
	terminalCalls, lookupCalls := 0, 0
	lookup := loom.NewArgsTool(loom.MustArgsContract("lookup"), "Look a term up.",
		func(context.Context, loom.Args) (string, error) { lookupCalls++; return `{}`, nil })
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "c1", Name: "finalize_answer"}}, FinishReason: loom.FinishReasonToolCalls},
		{ToolCalls: []loom.ToolCall{{ID: "c9", Name: "lookup"}}, FinishReason: loom.FinishReasonToolCalls},
	}}
	cfg := Config{Model: model, Tools: loom.NewToolRegistry(lookup, terminalTool(&terminalCalls))}
	result, err := runTerminalCase(t, cfg, "final-phase-tool-call")
	if !errors.Is(err, ErrToolCallInFinalPhase) {
		t.Fatalf("err = %v, want ErrToolCallInFinalPhase", err)
	}
	if result != nil {
		t.Fatalf("a failed final phase must not produce a result: %+v", result)
	}
	if lookupCalls != 0 {
		t.Fatalf("the call must not be executed in the final phase: %d", lookupCalls)
	}
}

// A terminal tool is the only way the phase can end, so no budget withholds it.
func TestTerminalToolSurvivesToolBudgets(t *testing.T) {
	t.Parallel()
	terminalCalls, lookupCalls := 0, 0
	lookup := loom.NewArgsTool(loom.MustArgsContract("lookup"), "Look a term up.",
		func(context.Context, loom.Args) (string, error) { lookupCalls++; return `{}`, nil })
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "c1", Name: "lookup"}}, FinishReason: loom.FinishReasonToolCalls},
		{ToolCalls: []loom.ToolCall{{ID: "c2", Name: "finalize_answer"}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "the answer", FinishReason: loom.FinishReasonStop},
	}}
	cfg := Config{Model: model, Tools: loom.NewToolRegistry(lookup, terminalTool(&terminalCalls)), MaxToolCalls: 1}
	result, err := runTerminalCase(t, cfg, "terminal-budget")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if lookupCalls != 1 || terminalCalls != 1 {
		t.Fatalf("lookup=%d terminal=%d, want 1 and 1", lookupCalls, terminalCalls)
	}
	var offered []string
	for _, info := range model.requests[1].Tools {
		offered = append(offered, info.Name)
	}
	if len(offered) != 1 || offered[0] != "finalize_answer" {
		t.Fatalf("an exhausted budget must still offer the terminal tool, got %v", offered)
	}
	if result == nil || result.EndedByTool != "finalize_answer" {
		t.Fatalf("result = %+v", result)
	}
}
