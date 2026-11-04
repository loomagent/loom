package react

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/loomagent/loom"
)

func runReservationTest(t *testing.T, ctx context.Context, cfg Config) (*loom.Turn, error) {
	t.Helper()
	cfg.Messages = []loom.Message{{Role: loom.RoleUser, Content: "test task"}}
	return loom.Run(ctx, func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		result, err := Run(ctx, w, cfg)
		if err != nil {
			return err
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: t.Name()})
}

func TestToolBatchReservationPrecedesExecutionAndPreservesModelOrder(t *testing.T) {
	type contextKey struct{}
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), contextKey{}, "scope"), time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var executed []string
	var batches []ToolReservationBatch
	tool := func(name string, options ...loom.ToolOption) loom.Tool {
		return loom.NewArgsTool(loom.MustArgsContract(name), name, func(context.Context, loom.Args) (string, error) {
			if len(batches) == 0 {
				t.Fatal("tool ran before reservation")
			}
			executed = append(executed, name)
			return "ok", nil
		}, options...)
	}
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "a", Name: "business"}, {ID: "b", Name: "business"}, {ID: "c", Name: "helper"}, {ID: "d", Name: "hidden"}}, FinishReason: loom.FinishReasonToolCalls},
		{ToolCalls: []loom.ToolCall{{ID: "e", Name: "helper"}}, FinishReason: loom.FinishReasonToolCalls},
		{ToolCalls: []loom.ToolCall{{ID: "f", Name: "finish"}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "answer", FinishReason: loom.FinishReasonStop},
	}}
	cfg := Config{Model: model, Tools: loom.NewToolRegistry(tool("business"), tool("helper", loom.WithBudgetExempt()), tool("hidden"), tool("finish")),
		Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}, MaxToolCalls: 1, MaxToolRounds: 1, TerminalToolName: "finish",
		StepPolicies: []StepPolicy{StepPolicyFunc(func(_ context.Context, _ State, plan *StepPlan) error {
			visible := plan.Tools[:0]
			for _, info := range plan.Tools {
				if info.Name != "hidden" {
					visible = append(visible, info)
				}
			}
			plan.Tools = visible
			return nil
		})},
		ReserveTools: func(ctx context.Context, batch ToolReservationBatch) error {
			if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) || ctx.Value(contextKey{}) != "scope" {
				t.Fatal("reservation lost execution context")
			}
			if batch.ToolCallsUsed != 1 || batch.ToolRoundsUsed != 1 || batch.ToolUses["business"] != 1 {
				t.Fatalf("wrong post-reservation budget: %+v", batch)
			}
			if batch.Step != uint64(len(batches)) {
				t.Fatalf("step=%d batches=%d", batch.Step, len(batches))
			}
			// Retaining and changing the supplied value cannot rewrite runtime
			// counters, accepted call identities, or later snapshots.
			retained := batch
			retained.ToolUses = maps.Clone(batch.ToolUses)
			retained.Reservations = append([]ToolReservation(nil), batch.Reservations...)
			batches = append(batches, retained)
			batch.ToolUses["business"] = 999
			batch.Reservations[0].Name = "mutated"
			return nil
		}}
	turn, err := runReservationTest(t, ctx, cfg)
	if err != nil || turn.Status != loom.TurnStatusCompleted {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
	if !reflect.DeepEqual(executed, []string{"business", "helper", "helper", "finish"}) || len(batches) != 3 {
		t.Fatalf("executed=%v batches=%+v", executed, batches)
	}
	want := []ToolReservation{{Index: 0, CallID: "a", Name: "business"}, {Index: 2, CallID: "c", Name: "helper", BudgetExempt: true}}
	if !reflect.DeepEqual(batches[0].Reservations, want) {
		t.Fatalf("accepted positions/classification=%+v", batches[0].Reservations)
	}
	if got := batches[2].Reservations; len(got) != 1 || !got[0].EndsToolPhase || !got[0].BudgetExempt {
		t.Fatalf("configured terminal classification=%+v", got)
	}
	if len(model.requests[len(model.requests)-1].Tools) != 0 {
		t.Fatal("terminal reservation reopened tools")
	}
}

func TestToolBatchReservationFailurePreventsAllAcceptedExecution(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "business_and_internal"
		if terminal {
			name = "terminal"
		}
		t.Run(name, func(t *testing.T) {
			failure := errors.New("reservation transaction unavailable")
			executed, scheduled, reserved := 0, false, 0
			tool := func(name string, options ...loom.ToolOption) loom.Tool {
				return loom.NewArgsTool(loom.MustArgsContract(name), name, func(context.Context, loom.Args) (string, error) { executed++; return "ok", nil }, options...)
			}
			calls := []loom.ToolCall{{ID: "a", Name: "business"}, {ID: "b", Name: "helper"}}
			if terminal {
				calls = []loom.ToolCall{{ID: "f", Name: "finish"}}
			}
			cfg := Config{Model: &scriptedModel{responses: []*loom.ChatResponse{{ToolCalls: calls, FinishReason: loom.FinishReasonToolCalls}}},
				Tools:     loom.NewToolRegistry(tool("business"), tool("helper", loom.WithBudgetExempt()), tool("finish", loom.WithEndsToolPhase())),
				Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
				ReserveTools: func(_ context.Context, batch ToolReservationBatch) error {
					reserved++
					if len(batch.Reservations) != len(calls) {
						t.Fatalf("partial reservation: %+v", batch)
					}
					return failure
				},
				ExecuteTools: func(context.Context, loom.Writer, State, *loom.ToolRegistry, []loom.ToolCall) ([]loom.ToolExecResult, error) {
					scheduled = true
					return nil, nil
				}}
			turn, err := runReservationTest(t, t.Context(), cfg)
			if !errors.Is(err, failure) || turn.Status != loom.TurnStatusFailed || executed != 0 || scheduled || reserved != 1 {
				t.Fatalf("status=%s err=%v executed=%d scheduled=%v reserved=%d", turn.Status, err, executed, scheduled, reserved)
			}
		})
	}
}

func TestRefusedToolBatchesAreNotReserved(t *testing.T) {
	for _, name := range []string{"unknown", "hidden", "mixed_terminal", "over_budget"} {
		t.Run(name, func(t *testing.T) {
			used, reserved := 0, 0
			business := loom.NewArgsTool(loom.MustArgsContract("business"), "business", func(context.Context, loom.Args) (string, error) { used++; return "ok", nil })
			terminal := loom.NewArgsTool(loom.MustArgsContract("finish"), "finish", func(context.Context, loom.Args) (string, error) { t.Fatal("refused terminal ran"); return "", nil }, loom.WithEndsToolPhase())
			calls := []loom.ToolCall{{ID: "a", Name: "missing"}}
			if name != "unknown" {
				calls[0].Name = "business"
			}
			if name == "mixed_terminal" {
				calls = append(calls, loom.ToolCall{ID: "b", Name: "finish"})
			}
			responses := []*loom.ChatResponse{{ToolCalls: calls, FinishReason: loom.FinishReasonToolCalls}, {Content: "answer", FinishReason: loom.FinishReasonStop}}
			if name == "over_budget" {
				responses = append([]*loom.ChatResponse{{ToolCalls: []loom.ToolCall{{ID: "first", Name: "business"}}, FinishReason: loom.FinishReasonToolCalls}}, responses...)
			}
			cfg := Config{Model: &scriptedModel{responses: responses}, Tools: loom.NewToolRegistry(business, terminal), MaxToolCalls: 1,
				Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}, ReserveTools: func(context.Context, ToolReservationBatch) error { reserved++; return nil }}
			if name == "hidden" {
				cfg.StepPolicies = []StepPolicy{StepPolicyFunc(func(_ context.Context, _ State, plan *StepPlan) error {
					for _, info := range plan.Tools {
						if info.Name == "finish" {
							plan.Tools = []*loom.ToolInfo{info}
							break
						}
					}
					return nil
				})}
			}
			turn, err := runReservationTest(t, t.Context(), cfg)
			want := 0
			if name == "over_budget" {
				want = 1
			}
			if err != nil || turn.Status != loom.TurnStatusCompleted || reserved != want || used != want {
				t.Fatalf("status=%s err=%v used=%d reserved=%d", turn.Status, err, used, reserved)
			}
		})
	}
}

func TestFailedExecutionKeepsItsRecordedReservation(t *testing.T) {
	reserved, used := 0, 0
	tool := loom.NewArgsTool(loom.MustArgsContract("business"), "business", func(context.Context, loom.Args) (string, error) {
		if reserved != 1 {
			t.Fatal("execution preceded reservation")
		}
		used++
		return "", errors.New("upstream failed")
	})
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "a", Name: "business"}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "cannot complete research", FinishReason: loom.FinishReasonStop},
	}}
	cfg := Config{Model: model, Tools: loom.NewToolRegistry(tool), MaxToolCalls: 1, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
		ReserveTools: func(_ context.Context, batch ToolReservationBatch) error {
			if batch.ToolCallsUsed != 1 || batch.ToolRoundsUsed != 1 || batch.ToolUses["business"] != 1 {
				t.Fatalf("reservation=%+v", batch)
			}
			reserved++
			return nil
		},
		AfterToolsPolicies: []AfterToolsPolicy{AfterToolsPolicyFunc(func(_ context.Context, _ loom.Writer, state State, results []loom.ToolExecResult) (AfterToolsDecision, error) {
			if state.ToolCallsUsed != 1 || state.ToolRoundsUsed != 1 || len(results) != 1 || results[0].Err == nil {
				t.Fatalf("failure refunded its reservation: state=%+v results=%+v", state, results)
			}
			return AfterToolsDecision{}, nil
		})}}
	turn, err := runReservationTest(t, t.Context(), cfg)
	if err != nil || turn.Status != loom.TurnStatusCompleted || used != 1 || reserved != 1 {
		t.Fatalf("status=%s err=%v used=%d reserved=%d", turn.Status, err, used, reserved)
	}
}
