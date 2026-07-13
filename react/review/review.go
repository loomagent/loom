// Package review provides a generic quality gate for ReAct tool rounds.
package review

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/react"
)

// Reviewer assesses accumulated context and the latest tool results.
type Reviewer interface {
	Review(ctx context.Context, request Request) (Assessment, error)
}

// ReviewerFunc adapts a function to Reviewer.
type ReviewerFunc func(context.Context, Request) (Assessment, error)

func (f ReviewerFunc) Review(ctx context.Context, request Request) (Assessment, error) {
	return f(ctx, request)
}

// Request is independent of any search provider or citation format.
type Request struct {
	Step        uint64
	Messages    []loom.Message
	ToolResults []loom.ToolExecResult
}

// Assessment is a reviewer's normalized decision.
type Assessment struct {
	Sufficient  bool
	Summary     string
	Instruction string
}

// Config controls a Policy.
type Config struct {
	Reviewer Reviewer
	// EveryToolRounds defaults to one.
	EveryToolRounds uint64
	// StopWhenSufficient stops immediately after a sufficient assessment.
	StopWhenSufficient bool
	// RequireReviewBeforeFinish rejects an early model finish until at least one
	// review has run and found the context sufficient.
	RequireReviewBeforeFinish bool
	// ContinueInstruction is used when an insufficient assessment omits one.
	ContinueInstruction string
	// ReasoningLabel defaults to "reviewer".
	ReasoningLabel string
}

// Policy implements react.AfterToolsPolicy and react.FinishPolicy.
// A Policy is stateful and should be created per ReAct run.
type Policy struct {
	cfg        Config
	mu         sync.Mutex
	rounds     uint64
	reviewed   bool
	sufficient bool
}

// New constructs a review policy.
func New(config Config) (*Policy, error) {
	if config.Reviewer == nil {
		return nil, errors.New("review: Reviewer is required")
	}
	if config.EveryToolRounds == 0 {
		config.EveryToolRounds = 1
	}
	if strings.TrimSpace(config.ReasoningLabel) == "" {
		config.ReasoningLabel = "reviewer"
	}
	return &Policy{cfg: config}, nil
}

func (p *Policy) AfterTools(ctx context.Context, w loom.Writer, state react.State, results []loom.ToolExecResult) (react.AfterToolsDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rounds++
	if p.rounds%p.cfg.EveryToolRounds != 0 {
		return react.AfterToolsDecision{}, nil
	}
	assessment, err := p.cfg.Reviewer.Review(ctx, Request{
		Step:        state.Step,
		Messages:    append([]loom.Message(nil), state.Messages...),
		ToolResults: append([]loom.ToolExecResult(nil), results...),
	})
	if err != nil {
		return react.AfterToolsDecision{}, err
	}
	p.reviewed = true
	p.sufficient = assessment.Sufficient
	if summary := strings.TrimSpace(assessment.Summary); summary != "" {
		if err := w.WriteReasoning(ctx, p.cfg.ReasoningLabel, summary); err != nil {
			return react.AfterToolsDecision{}, err
		}
	}
	if assessment.Sufficient && p.cfg.StopWhenSufficient {
		return react.AfterToolsDecision{Stop: true, FinalContent: assessment.Summary}, nil
	}
	if !assessment.Sufficient {
		instruction := strings.TrimSpace(assessment.Instruction)
		if instruction == "" {
			instruction = strings.TrimSpace(p.cfg.ContinueInstruction)
		}
		if instruction != "" {
			messages := append([]loom.Message(nil), state.Messages...)
			messages = append(messages, loom.Message{Role: loom.RoleSystem, Content: instruction})
			return react.AfterToolsDecision{Messages: messages}, nil
		}
	}
	return react.AfterToolsDecision{}, nil
}

func (p *Policy) BeforeFinish(_ context.Context, _ react.State, _ *loom.ChatResponse) (react.FinishDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sufficient || (!p.cfg.RequireReviewBeforeFinish && !p.reviewed) {
		return react.FinishDecision{}, nil
	}
	instruction := strings.TrimSpace(p.cfg.ContinueInstruction)
	if instruction == "" {
		instruction = "Continue using the available tools until the review criteria are satisfied."
	}
	return react.FinishDecision{Continue: true, Instruction: instruction}, nil
}

var (
	_ react.AfterToolsPolicy = (*Policy)(nil)
	_ react.FinishPolicy     = (*Policy)(nil)
)
