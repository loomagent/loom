package contextpolicy

import (
	"context"
	"errors"
	"testing"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/react"
)

func TestChain(t *testing.T) {
	appendMessage := func(content string) Policy {
		return Func(func(_ context.Context, input Input) (Result, error) {
			return Result{
				Messages:  append(input.Messages, loom.Message{Role: loom.RoleSystem, Content: content}),
				Decisions: []Decision{{Policy: content, Action: "append"}},
			}, nil
		})
	}
	result, err := (Chain{appendMessage("one"), appendMessage("two")}).Build(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 || result.Messages[1].Content != "two" || len(result.Decisions) != 2 {
		t.Fatalf("result = %+v", result)
	}
}

func TestHistoryAppendsUserMessage(t *testing.T) {
	result, err := History(t.Context(), Input{User: loom.UserMessage{Text: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("messages = %+v", result.Messages)
	}
	if got := result.Messages[0]; got.Role != loom.RoleUser || got.Content != "hello" {
		t.Fatalf("message = %+v", got)
	}
}

// A history turn that never completed cannot be handed to the model as context.
func TestHistoryRejectsUnfinishedTurn(t *testing.T) {
	_, err := History(t.Context(), Input{History: []loom.Turn{{Index: 3, Status: loom.TurnStatusInProgress}}})
	if err == nil {
		t.Fatal("in-progress history turn accepted")
	}
}

func TestReactStepPolicy(t *testing.T) {
	// A nil policy leaves the plan alone.
	plan := &react.StepPlan{Messages: []loom.Message{{Role: loom.RoleUser, Content: "keep"}}}
	if err := (ReactStepPolicy{}).PrepareStep(t.Context(), react.State{}, plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Messages) != 1 || plan.Messages[0].Content != "keep" {
		t.Fatalf("plan changed by an absent policy: %+v", plan.Messages)
	}

	var decisions []Decision
	metadata := map[string]string{"k": "v"}
	policy := ReactStepPolicy{
		Policy: Func(func(_ context.Context, input Input) (Result, error) {
			// The policy receives a copy, so this write must not reach the caller.
			input.Metadata["k"] = "mutated"
			return Result{
				Messages:  append(input.Messages, loom.Message{Role: loom.RoleSystem, Content: "added"}),
				Decisions: []Decision{{Policy: "p", Action: "add"}},
			}, nil
		}),
		Metadata:      metadata,
		FirstStepOnly: true,
		OnDecisions:   func(reported []Decision) { decisions = append(decisions, reported...) },
	}

	plan = &react.StepPlan{Messages: []loom.Message{{Role: loom.RoleUser, Content: "start"}}}
	if err := policy.PrepareStep(t.Context(), react.State{Step: 0}, plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Messages) != 2 || plan.Messages[1].Content != "added" {
		t.Fatalf("plan messages = %+v", plan.Messages)
	}
	if len(decisions) != 1 || decisions[0].Action != "add" {
		t.Fatalf("decisions = %+v", decisions)
	}
	if metadata["k"] != "v" {
		t.Fatalf("policy mutated caller metadata: %v", metadata)
	}

	// FirstStepOnly: later steps are skipped entirely.
	plan = &react.StepPlan{Messages: []loom.Message{{Role: loom.RoleUser, Content: "later"}}}
	if err := policy.PrepareStep(t.Context(), react.State{Step: 2}, plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Messages) != 1 || plan.Messages[0].Content != "later" {
		t.Fatalf("policy ran after the first step: %+v", plan.Messages)
	}
}

// A nil metadata map reaches the policy as nil rather than as an empty map.
func TestReactStepPolicyKeepsNilMetadata(t *testing.T) {
	policy := ReactStepPolicy{Policy: Func(func(_ context.Context, input Input) (Result, error) {
		if input.Metadata != nil {
			t.Errorf("nil metadata became %v", input.Metadata)
		}
		return Result{}, nil
	})}
	if err := policy.PrepareStep(t.Context(), react.State{}, &react.StepPlan{}); err != nil {
		t.Fatal(err)
	}
}

func TestReactStepPolicyPropagatesError(t *testing.T) {
	wantErr := errors.New("policy failed")
	policy := ReactStepPolicy{Policy: Func(func(context.Context, Input) (Result, error) {
		return Result{}, wantErr
	})}
	if err := policy.PrepareStep(t.Context(), react.State{}, &react.StepPlan{}); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}
