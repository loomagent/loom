package react

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/loomagent/loom"
)

func TestUserCustomTerminalNamesShareOneFinalizationProtocol(t *testing.T) {
	for _, name := range []string{"finalize_answer", "start_report", "publish_summary"} {
		for _, scenario := range []string{"standalone", "mixed_batch", "failed_then_success"} {
			t.Run("user_"+name+"_"+scenario, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					// Given an ordinary registered tool assigned the terminal role by configuration.
					research, attempts, successes := 0, 0, 0
					lookup := loom.NewArgsTool(loom.MustArgsContract("lookup"), "lookup", func(context.Context, loom.Args) (string, error) { research++; return "evidence", nil })
					terminal := loom.NewArgsTool(loom.MustArgsContract(name), "end research", func(context.Context, loom.Args) (string, error) {
						attempts++
						if scenario == "failed_then_success" && attempts == 1 {
							return "partial", errors.New("not ready")
						}
						successes++
						return "ready", nil
					})
					registry := loom.NewToolRegistry(lookup, terminal)
					call := func(id, tool string) *loom.ChatResponse {
						return &loom.ChatResponse{ToolCalls: []loom.ToolCall{{ID: id, Name: tool}}, FinishReason: loom.FinishReasonToolCalls}
					}
					responses := []*loom.ChatResponse{call("lookup", "lookup")}
					if scenario == "mixed_batch" {
						responses = append(responses, &loom.ChatResponse{ToolCalls: []loom.ToolCall{{ID: "mixed_lookup", Name: "lookup"}, {ID: "mixed_terminal", Name: name}}, FinishReason: loom.FinishReasonToolCalls})
					}
					if scenario == "failed_then_success" {
						responses = append(responses, call("failed", name))
					}
					responses = append(responses, call("finish", name), &loom.ChatResponse{ReasoningContent: "final reasoning", Content: "answer", FinishReason: loom.FinishReasonStop})
					model := &scriptedModel{responses: responses}
					var result *Result
					// When the budget runs out, the configured terminal remains available.
					turn, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
						var err error
						result, err = RunToFinalAnswer(ctx, w, Config{Model: model, Tools: registry, TerminalToolName: name, MaxToolCalls: 1, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}})
						return err
					}, loom.RunOptions{ConversationID: "custom-terminal"})
					// Then all names have the same isolation, failure and one-success semantics.
					if err != nil || turn.Status != loom.TurnStatusCompleted || result == nil || !result.FinalAnswerCommitted || result.EndedByTool != name {
						t.Fatalf("turn=%+v result=%+v err=%v", turn, result, err)
					}
					if research != 1 || successes != 1 {
						t.Fatalf("research=%d successful terminations=%d", research, successes)
					}
					wantAttempts := 1
					if scenario == "failed_then_success" {
						wantAttempts = 2
					}
					if attempts != wantAttempts {
						t.Fatalf("terminal attempts=%d", attempts)
					}
					last := model.requests[len(model.requests)-1]
					if len(last.Tools) != 0 {
						t.Fatal("final round exposes tools")
					}
					var reasoning, answer string
					for _, item := range turn.Items {
						if item.Kind == loom.ItemKindReasoning {
							reasoning += item.Text
						}
						if item.Kind == loom.ItemKindFinalAnswer {
							answer += item.Text
						}
					}
					if reasoning != "final reasoning" || answer != "answer" {
						t.Fatalf("reasoning=%q answer=%q", reasoning, answer)
					}
					info, err := terminal.Info(t.Context())
					if err != nil || info.EndsToolPhase {
						t.Fatal("per-run naming mutated the shared tool")
					}
				})
			})
		}
	}
}
