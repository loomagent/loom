package react

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/loomagent/loom"
)

func TestInternalToolsPreserveResearchBudgets(t *testing.T) {
	t.Parallel()
	for _, budget := range []string{"rounds", "total", "per_tool"} {
		t.Run(budget, func(t *testing.T) {
			t.Parallel()
			sequence := 0
			calls := func(names ...string) *loom.ChatResponse {
				r := &loom.ChatResponse{FinishReason: loom.FinishReasonToolCalls, Usage: loom.Usage{PromptTokens: 2, TotalTokens: 2}}
				for _, name := range names {
					sequence++
					r.ToolCalls = append(r.ToolCalls, loom.ToolCall{ID: fmt.Sprintf("%s-%d", name, sequence), Name: name, Arguments: `{}`})
				}
				return r
			}
			model := &scriptedModel{responses: []*loom.ChatResponse{
				calls("lookup", "archive_read"), calls("archive_read"), calls("lookup", "lookup"),
				calls("lookup", "archive_read"), calls("finish"),
				{Content: "answer", FinishReason: loom.FinishReasonStop, Usage: loom.Usage{PromptTokens: 2, TotalTokens: 2}},
			}}
			research, reads := 0, 0
			tools := loom.NewToolRegistry(
				loom.NewArgsTool(loom.MustArgsContract("lookup"), "lookup", func(context.Context, loom.Args) (string, error) { research++; return "evidence", nil }),
				// Networking and budget classification are independent.
				loom.NewArgsTool(loom.MustArgsContract("archive_read"), "read", func(context.Context, loom.Args) (string, error) { reads++; return "saved evidence", nil }, loom.WithBudgetExempt(), loom.WithRequiresNetwork()),
				loom.NewArgsTool(loom.MustArgsContract("finish"), "finish", func(context.Context, loom.Args) (string, error) { return "ok", nil }, loom.WithEndsToolPhase()),
			)
			cfg := Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "research"}}, Model: model, Tools: tools, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}, MaxSteps: 10, TerminalToolName: "finish", ToolCallLimits: map[string]uint64{}}
			wantResearch := 2
			switch budget {
			case "rounds":
				cfg.MaxToolRounds = 2
				wantResearch = 3
			case "total":
				cfg.MaxToolCalls = 2
			case "per_tool":
				cfg.ToolCallLimits["lookup"] = 2
			default:
				t.Fatal(budget)
			}
			var observed State
			cfg.AfterToolsPolicies = []AfterToolsPolicy{AfterToolsPolicyFunc(func(_ context.Context, _ loom.Writer, s State, _ []loom.ToolExecResult) (AfterToolsDecision, error) {
				observed = s
				return AfterToolsDecision{}, nil
			})}
			var result *Result
			turn, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
				var e error
				result, e = RunToFinalAnswer(ctx, w, cfg)
				return e
			}, loom.RunOptions{ConversationID: "budget"})
			if err != nil {
				t.Fatal(err)
			}
			if research != wantResearch || reads != 3 || observed.ToolRoundsUsed != 2 || observed.ToolCallsUsed != uint64(wantResearch) || observed.ToolUses["archive_read"] != 0 {
				t.Fatalf("research=%d reads=%d state=%+v", research, reads, observed)
			}
			if result == nil || !result.FinalAnswerCommitted || len(model.requests) != 6 || turn.Usage.PromptTokens != 12 {
				t.Fatalf("result=%+v requests=%d usage=%+v", result, len(model.requests), turn.Usage)
			}
			last := model.requests[5]
			if len(last.Tools) != 0 || last.ToolChoice == nil || last.ToolChoice.Mode != loom.ToolChoiceNone {
				t.Fatalf("final request=%+v", last)
			}
			// The fourth request has no business tools, but reads and terminal remain.
			for _, info := range model.requests[3].Tools {
				if info.Name == "lookup" {
					t.Fatal("exhausted tool still exposed")
				}
			}
			found := false
			for _, msg := range last.Messages {
				if msg.Role == loom.RoleTool && msg.ToolCallID == "lookup-6" && msg.Content != "evidence" {
					found = true
				}
			}
			if !found {
				t.Fatal("over-budget response missing from history")
			}
		})
	}
}

func TestInternalToolsRemainBoundedByMaxSteps(t *testing.T) {
	t.Parallel()
	count := 0
	tool := loom.NewArgsTool(loom.MustArgsContract("read"), "read", func(context.Context, loom.Args) (string, error) { count++; return "evidence", nil }, loom.WithBudgetExempt())
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "one", Name: "read", Arguments: `{}`}}, FinishReason: loom.FinishReasonToolCalls},
		{ToolCalls: []loom.ToolCall{{ID: "two", Name: "read", Arguments: `{}`}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "bounded answer", FinishReason: loom.FinishReasonStop},
	}}
	var result *Result
	_, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var e error
		result, e = RunToFinalAnswer(ctx, w, Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "research"}}, Model: model, Tools: loom.NewToolRegistry(tool), Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}, MaxSteps: 2, MaxToolRounds: 1})
		return e
	}, loom.RunOptions{ConversationID: "bounded"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || result.FinalizationReason != "step_limit" || !result.SoftLanded || len(model.requests[2].Tools) != 0 {
		t.Fatalf("calls=%d result=%+v", count, result)
	}
}

func TestToolRoundLimitCannotBeReopenedByPolicy(t *testing.T) {
	t.Parallel()
	count := 0
	tool := loom.NewArgsTool(loom.MustArgsContract("lookup"), "lookup", func(context.Context, loom.Args) (string, error) {
		count++
		return "evidence", errors.New("failed lookup")
	})
	read := loom.NewArgsTool(loom.MustArgsContract("read"), "read", func(context.Context, loom.Args) (string, error) { return "saved", nil }, loom.WithBudgetExempt())
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "one", Name: "lookup", Arguments: `{}`}}, FinishReason: loom.FinishReasonToolCalls},
		{ToolCalls: []loom.ToolCall{{ID: "two", Name: "lookup", Arguments: `{}`}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "answer", FinishReason: loom.FinishReasonStop},
	}}
	cfg := Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "research"}}, Model: model, Tools: loom.NewToolRegistry(tool, read), MaxSteps: 4, MaxToolRounds: 1, StepPolicies: []StepPolicy{StepPolicyFunc(func(_ context.Context, s State, p *StepPlan) error { p.Tools = s.ToolInfos; return nil })}}
	_, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		result, e := Run(ctx, w, cfg)
		if e != nil {
			return e
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "policy"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("failed business reservations/policy bypass: calls=%d", count)
	}
}

func TestBudgetExemptToolLimitIsRejected(t *testing.T) {
	t.Parallel()
	tool := loom.NewArgsTool(loom.MustArgsContract("archive_read"), "read", func(context.Context, loom.Args) (string, error) { return "saved", nil }, loom.WithBudgetExempt())
	cfg := Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "research"}}, Model: &scriptedModel{}, Tools: loom.NewToolRegistry(tool), Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}, ToolCallLimits: map[string]uint64{"archive_read": 1}}
	_, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		_, e := Run(ctx, w, cfg)
		return e
	}, loom.RunOptions{ConversationID: "exempt-limit"})
	if err == nil || !strings.Contains(err.Error(), `budget-exempt tool "archive_read" cannot have a tool limit`) {
		t.Fatalf("config conflict accepted: %v", err)
	}
}
