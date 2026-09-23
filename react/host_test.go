package react

import (
	"context"
	"errors"
	"testing"

	"github.com/loomagent/loom"
)

func TestUserHostSchedulerCannotExecuteHiddenOrOverBudgetTools(t *testing.T) {
	// Given a visible lookup, a hidden tool and only one research reservation.
	used := 0
	tool := loom.NewArgsTool(loom.MustArgsContract("lookup"), "lookup", func(context.Context, loom.Args) (string, error) { used++; return "evidence", nil })
	hidden := loom.NewArgsTool(loom.MustArgsContract("hidden"), "hidden", func(context.Context, loom.Args) (string, error) { t.Error("hidden tool ran"); return "", nil })
	m := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "a", Name: "lookup"}, {ID: "b", Name: "lookup"}, {ID: "c", Name: "hidden"}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "answer", FinishReason: loom.FinishReasonStop},
	}}
	var result *Result
	_, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var err error
		// When a host scheduler executes through the public runtime.
		result, err = Run(ctx, w, Config{Model: m, Tools: loom.NewToolRegistry(tool, hidden), Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}, MaxToolCalls: 1,
			StepPolicies: []StepPolicy{StepPolicyFunc(func(_ context.Context, s State, p *StepPlan) error {
				p.Tools = []*loom.ToolInfo{s.ToolInfos[0]}
				return nil
			})},
			ExecuteTools: func(ctx context.Context, w loom.Writer, _ State, r *loom.ToolRegistry, calls []loom.ToolCall) ([]loom.ToolExecResult, error) {
				return loom.ExecuteToolCalls(ctx, w, r, calls)
			},
		})
		if err != nil {
			return err
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "host-budget"})
	// Then one side effect happens and every requested call has a paired result.
	if err != nil || used != 1 || result == nil {
		t.Fatalf("used=%d result=%+v err=%v", used, result, err)
	}
	ids := map[string]bool{}
	for _, msg := range result.Messages {
		if msg.Role == loom.RoleTool {
			ids[msg.ToolCallID] = true
		}
	}
	if len(ids) != 3 {
		t.Fatalf("missing results: %v", ids)
	}
}

func TestUserMalformedHostResultsFailWithoutDeliveringAnAnswer(t *testing.T) {
	for _, kind := range []string{"missing", "identity"} {
		t.Run("user_"+kind, func(t *testing.T) {
			// Given an adapter returning incomplete or mismatched tool results.
			tool := loom.NewArgsTool(loom.MustArgsContract("lookup"), "lookup", func(context.Context, loom.Args) (string, error) { return "ok", nil })
			m := &scriptedModel{responses: []*loom.ChatResponse{{ToolCalls: []loom.ToolCall{{ID: "a", Name: "lookup"}}, FinishReason: loom.FinishReasonToolCalls}}}
			turn, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
				_, err := Run(ctx, w, Config{Model: m, Tools: loom.NewToolRegistry(tool), Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}, ExecuteTools: func(context.Context, loom.Writer, State, *loom.ToolRegistry, []loom.ToolCall) ([]loom.ToolExecResult, error) {
					if kind == "missing" {
						return nil, nil
					}
					return []loom.ToolExecResult{{Call: loom.ToolCall{ID: "wrong", Name: "lookup"}}}, nil
				}})
				return err
			}, loom.RunOptions{ConversationID: "bad-host"})
			// Then failure is typed and no answer is committed.
			if !errors.Is(err, ErrInvalidToolExecution) || turn.Status != loom.TurnStatusFailed {
				t.Fatalf("turn=%+v err=%v", turn, err)
			}
		})
	}
}

func TestUserHostCallFailuresNeverCommitSuccess(t *testing.T) {
	for _, kind := range []string{"nil_response", "model_error", "executor_error", "invalid_visible_tool", "unknown_terminal", "limited_terminal", "ambiguous_fallback"} {
		t.Run("user_"+kind, func(t *testing.T) {
			// Given an invalid host configuration or a failed dependency.
			dependencyErr := errors.New("dependency unavailable")
			tool := loom.NewArgsTool(loom.MustArgsContract("lookup"), "lookup", func(context.Context, loom.Args) (string, error) { return "ok", nil })
			m := &scriptedModel{responses: []*loom.ChatResponse{{ToolCalls: []loom.ToolCall{{ID: "a", Name: "lookup"}}, FinishReason: loom.FinishReasonToolCalls}}}
			cfg := Config{Model: m, Tools: loom.NewToolRegistry(tool), Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}}
			switch kind {
			case "nil_response":
				cfg.CallModel = func(context.Context, loom.Writer, State, string, loom.ChatModel, loom.ChatRequest) (*loom.ChatResponse, error) {
					return nil, nil
				}
			case "model_error":
				cfg.CallModel = func(context.Context, loom.Writer, State, string, loom.ChatModel, loom.ChatRequest) (*loom.ChatResponse, error) {
					return nil, dependencyErr
				}
			case "executor_error":
				cfg.ExecuteTools = func(context.Context, loom.Writer, State, *loom.ToolRegistry, []loom.ToolCall) ([]loom.ToolExecResult, error) {
					return nil, dependencyErr
				}
			case "invalid_visible_tool":
				cfg.StepPolicies = []StepPolicy{StepPolicyFunc(func(_ context.Context, _ State, p *StepPlan) error {
					p.Tools = []*loom.ToolInfo{{Name: "missing"}}
					return nil
				})}
			case "unknown_terminal":
				cfg.TerminalToolName = "missing"
			case "limited_terminal":
				cfg.TerminalToolName = "lookup"
				cfg.ToolCallLimits = map[string]uint64{"lookup": 1}
			case "ambiguous_fallback":
				cfg.SensitiveFallback = &SensitiveFallbackConfig{Model: m}
				cfg.CallModel = func(context.Context, loom.Writer, State, string, loom.ChatModel, loom.ChatRequest) (*loom.ChatResponse, error) {
					return nil, nil
				}
			}
			// When run through the public entry point, then the turn fails explicitly.
			turn, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
				_, err := Run(ctx, w, cfg)
				return err
			}, loom.RunOptions{ConversationID: "failed-host"})
			if err == nil || turn.Status != loom.TurnStatusFailed {
				t.Fatalf("turn=%+v err=%v", turn, err)
			}
			if (kind == "model_error" || kind == "executor_error") && !errors.Is(err, dependencyErr) {
				t.Fatalf("lost cause: %v", err)
			}
		})
	}
}
