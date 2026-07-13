// Package contextpolicy defines composable, application-neutral policies for
// assembling model context.
package contextpolicy

import (
	"context"
	"fmt"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/react"
)

// Input is the context visible to a Policy.
type Input struct {
	Step     uint64
	Messages []loom.Message
	History  []loom.Turn
	User     loom.UserMessage
	Metadata map[string]string
}

// Result contains the next message list and optional audit decisions.
type Result struct {
	Messages  []loom.Message
	Decisions []Decision
}

// Decision is an application-defined, serializable explanation of a context
// transformation.
type Decision struct {
	Policy string `json:"policy"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// Policy transforms context without knowing how history or external state is
// stored.
type Policy interface {
	Build(ctx context.Context, input Input) (Result, error)
}

// Func adapts a function to Policy.
type Func func(context.Context, Input) (Result, error)

func (f Func) Build(ctx context.Context, input Input) (Result, error) {
	return f(ctx, input)
}

// Chain applies policies in order. Each policy receives the messages produced
// by the previous one; decisions are accumulated.
type Chain []Policy

func (c Chain) Build(ctx context.Context, input Input) (Result, error) {
	messages := append([]loom.Message(nil), input.Messages...)
	var decisions []Decision
	for i, policy := range c {
		if policy == nil {
			continue
		}
		input.Messages = messages
		result, err := policy.Build(ctx, input)
		if err != nil {
			return Result{}, fmt.Errorf("contextpolicy: policy %d: %w", i, err)
		}
		if result.Messages != nil {
			messages = result.Messages
		}
		decisions = append(decisions, result.Decisions...)
	}
	return Result{Messages: messages, Decisions: decisions}, nil
}

// History builds ordinary model messages from completed Loom turns and the
// current user message.
func History(ctx context.Context, input Input) (Result, error) {
	_ = ctx
	messages, err := loom.HistoryToMessages(input.History, input.User)
	if err != nil {
		return Result{}, err
	}
	return Result{Messages: messages}, nil
}

// ReactStepPolicy adapts a Policy to react.StepPolicy. History, User, and
// Metadata are stable inputs supplied by the caller; current messages and step
// come from the running loop.
type ReactStepPolicy struct {
	Policy   Policy
	History  []loom.Turn
	User     loom.UserMessage
	Metadata map[string]string
	// FirstStepOnly applies the policy only when state.Step is zero.
	FirstStepOnly bool
	// OnDecisions receives audit decisions after a successful transformation.
	OnDecisions func([]Decision)
}

func (p ReactStepPolicy) PrepareStep(ctx context.Context, state react.State, plan *react.StepPlan) error {
	if p.Policy == nil || (p.FirstStepOnly && state.Step != 0) {
		return nil
	}
	result, err := p.Policy.Build(ctx, Input{
		Step:     state.Step,
		Messages: plan.Messages,
		History:  p.History,
		User:     p.User,
		Metadata: cloneMap(p.Metadata),
	})
	if err != nil {
		return err
	}
	if result.Messages != nil {
		plan.Messages = result.Messages
	}
	if p.OnDecisions != nil && len(result.Decisions) > 0 {
		p.OnDecisions(append([]Decision(nil), result.Decisions...))
	}
	return nil
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
