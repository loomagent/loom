// Package react provides a provider-neutral ReAct loop for Loom agents.
package react

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/loomagent/loom"
)

// Config controls a ReAct run.
type Config struct {
	Model    loom.ChatModel
	Tools    *loom.ToolRegistry
	Messages []loom.Message

	// Reasoning is sent on every model call. Its zero value defaults to enabled.
	Reasoning loom.Reasoning
	// MaxSteps is the number of tool-capable model rounds. When reached, Run
	// performs one final tool-free soft-landing call. Zero means unlimited.
	MaxSteps uint64
	// MaxToolCalls limits successful tool reservations across the run.
	MaxToolCalls uint64
	// ToolCallLimits applies per-tool limits. Missing and zero values are unlimited.
	ToolCallLimits map[string]uint64
	// SoftLandingPrompt is appended as a system message before the final call.
	SoftLandingPrompt string
	// Purpose prefixes model call span names and errors.
	Purpose string

	StepPolicies       []StepPolicy
	AfterToolsPolicies []AfterToolsPolicy
	FinishPolicies     []FinishPolicy
	// TransformToolResults may compact or redact results before they are sent
	// back to the model. The original results remain visible to policies.
	TransformToolResults func([]loom.ToolExecResult) []loom.ToolExecResult
}

// State is the observable state supplied to policies.
type State struct {
	Step          uint64
	Messages      []loom.Message
	ToolInfos     []*loom.ToolInfo
	ToolCallsUsed uint64
	ToolUses      map[string]uint64
}

// StepPlan describes the next model call and may be modified by StepPolicy.
type StepPlan struct {
	Messages    []loom.Message
	Tools       []*loom.ToolInfo
	ToolChoice  *loom.ToolChoice
	IsFinalStep bool
}

// StepPolicy runs before every model call.
type StepPolicy interface {
	PrepareStep(ctx context.Context, state State, plan *StepPlan) error
}

// StepPolicyFunc adapts a function to StepPolicy.
type StepPolicyFunc func(context.Context, State, *StepPlan) error

func (f StepPolicyFunc) PrepareStep(ctx context.Context, state State, plan *StepPlan) error {
	return f(ctx, state, plan)
}

// AfterToolsPolicy runs after a model round's tools have completed.
type AfterToolsPolicy interface {
	AfterTools(ctx context.Context, w loom.Writer, state State, results []loom.ToolExecResult) (AfterToolsDecision, error)
}

// AfterToolsPolicyFunc adapts a function to AfterToolsPolicy.
type AfterToolsPolicyFunc func(context.Context, loom.Writer, State, []loom.ToolExecResult) (AfterToolsDecision, error)

func (f AfterToolsPolicyFunc) AfterTools(ctx context.Context, w loom.Writer, state State, results []loom.ToolExecResult) (AfterToolsDecision, error) {
	return f(ctx, w, state, results)
}

// AfterToolsDecision may replace the accumulated messages or stop the loop.
type AfterToolsDecision struct {
	Messages     []loom.Message
	Stop         bool
	FinalContent string
}

// FinishPolicy runs when a model attempts to finish without tool calls.
type FinishPolicy interface {
	BeforeFinish(ctx context.Context, state State, response *loom.ChatResponse) (FinishDecision, error)
}

// FinishPolicyFunc adapts a function to FinishPolicy.
type FinishPolicyFunc func(context.Context, State, *loom.ChatResponse) (FinishDecision, error)

func (f FinishPolicyFunc) BeforeFinish(ctx context.Context, state State, response *loom.ChatResponse) (FinishDecision, error) {
	return f(ctx, state, response)
}

// FinishDecision may reject a finish and continue with an additional system
// instruction.
type FinishDecision struct {
	Continue    bool
	Instruction string
}

// Result describes the completed loop.
type Result struct {
	FinalContent string
	Messages     []loom.Message
	SoftLanded   bool
	Steps        uint64
}

// Run executes a ReAct loop, streaming model output and tool events through w.
// It does not call Writer.FinalAnswer; the caller owns the surrounding Turn's
// completion semantics.
func Run(ctx context.Context, w loom.Writer, cfg Config) (*Result, error) {
	if cfg.Model == nil {
		return nil, errors.New("react: Model is required")
	}
	if cfg.Tools == nil {
		return nil, errors.New("react: Tools is required")
	}
	if w == nil {
		return nil, errors.New("react: Writer is required")
	}
	purpose := strings.TrimSpace(cfg.Purpose)
	if purpose == "" {
		purpose = "react"
	}
	reasoning := cfg.Reasoning
	if reasoning.Mode == "" {
		reasoning.Mode = loom.ReasoningModeEnabled
	}

	allTools, err := cfg.Tools.InfoList(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: list tools: %w", purpose, err)
	}
	msgs := append([]loom.Message(nil), cfg.Messages...)
	uses := make(map[string]uint64, len(cfg.ToolCallLimits))
	var totalUses uint64

	for step := uint64(0); ; step++ {
		state := snapshotState(step, msgs, allTools, totalUses, uses)
		plan := StepPlan{
			Messages: append([]loom.Message(nil), msgs...),
			Tools:    availableTools(allTools, cfg, totalUses, uses),
		}
		if cfg.MaxSteps > 0 && step >= cfg.MaxSteps {
			plan.IsFinalStep = true
			plan.Tools = nil
			plan.ToolChoice = &loom.ToolChoice{Mode: loom.ToolChoiceNone}
			if prompt := strings.TrimSpace(cfg.SoftLandingPrompt); prompt != "" {
				plan.Messages = append(plan.Messages, loom.Message{Role: loom.RoleSystem, Content: prompt})
			}
		}
		for _, policy := range cfg.StepPolicies {
			if policy != nil {
				if err := policy.PrepareStep(ctx, state, &plan); err != nil {
					return nil, fmt.Errorf("%s: step %d policy: %w", purpose, step+1, err)
				}
			}
		}
		if len(plan.Tools) == 0 && plan.ToolChoice == nil {
			plan.ToolChoice = &loom.ToolChoice{Mode: loom.ToolChoiceNone}
		}
		state.Messages = append([]loom.Message(nil), plan.Messages...)

		response, err := loom.StreamLLMToStep(ctx, w, fmt.Sprintf("%s.step_%d", purpose, step+1), cfg.Model, loom.ChatRequest{
			Messages:   plan.Messages,
			Tools:      plan.Tools,
			ToolChoice: plan.ToolChoice,
			Reasoning:  reasoning,
		})
		if err != nil {
			return nil, fmt.Errorf("%s: step %d model: %w", purpose, step+1, err)
		}
		if err := validateFinishReason(response.FinishReason); err != nil {
			return nil, fmt.Errorf("%s: step %d: %w", purpose, step+1, err)
		}
		if plan.IsFinalStep {
			return &Result{FinalContent: response.Content, Messages: appendResponse(plan.Messages, response), SoftLanded: true, Steps: step + 1}, nil
		}

		if len(response.ToolCalls) == 0 {
			continued := false
			for _, policy := range cfg.FinishPolicies {
				if policy == nil {
					continue
				}
				decision, err := policy.BeforeFinish(ctx, state, response)
				if err != nil {
					return nil, fmt.Errorf("%s: step %d finish policy: %w", purpose, step+1, err)
				}
				if decision.Continue {
					msgs = appendResponse(plan.Messages, response)
					if instruction := strings.TrimSpace(decision.Instruction); instruction != "" {
						msgs = append(msgs, loom.Message{Role: loom.RoleSystem, Content: instruction})
					}
					continued = true
					break
				}
			}
			if continued {
				continue
			}
			return &Result{FinalContent: response.Content, Messages: appendResponse(plan.Messages, response), Steps: step + 1}, nil
		}

		results := make([]loom.ToolExecResult, 0, len(response.ToolCalls))
		for _, call := range response.ToolCalls {
			if reason := reserveTool(call.Name, cfg, &totalUses, uses); reason != "" {
				result, err := rejectToolCall(ctx, w, call, reason)
				if err != nil {
					return nil, fmt.Errorf("%s: step %d reject tool: %w", purpose, step+1, err)
				}
				results = append(results, result)
				continue
			}
			one, err := loom.ExecuteToolCalls(ctx, w, cfg.Tools, []loom.ToolCall{call})
			if err != nil {
				return nil, fmt.Errorf("%s: step %d execute tool: %w", purpose, step+1, err)
			}
			results = append(results, one...)
		}
		promptResults := results
		if cfg.TransformToolResults != nil {
			promptResults = cfg.TransformToolResults(append([]loom.ToolExecResult(nil), results...))
		}
		msgs = loom.AppendAssistantTurn(plan.Messages, response, promptResults)

		state = snapshotState(step, msgs, allTools, totalUses, uses)
		for _, policy := range cfg.AfterToolsPolicies {
			if policy == nil {
				continue
			}
			decision, err := policy.AfterTools(ctx, w, state, results)
			if err != nil {
				return nil, fmt.Errorf("%s: step %d after-tools policy: %w", purpose, step+1, err)
			}
			if decision.Messages != nil {
				msgs = decision.Messages
				state.Messages = msgs
			}
			if decision.Stop {
				return &Result{FinalContent: decision.FinalContent, Messages: msgs, Steps: step + 1}, nil
			}
		}
	}
}

func snapshotState(step uint64, messages []loom.Message, tools []*loom.ToolInfo, total uint64, uses map[string]uint64) State {
	useCopy := make(map[string]uint64, len(uses))
	for name, count := range uses {
		useCopy[name] = count
	}
	return State{Step: step, Messages: append([]loom.Message(nil), messages...), ToolInfos: append([]*loom.ToolInfo(nil), tools...), ToolCallsUsed: total, ToolUses: useCopy}
}

func availableTools(all []*loom.ToolInfo, cfg Config, total uint64, uses map[string]uint64) []*loom.ToolInfo {
	if cfg.MaxToolCalls > 0 && total >= cfg.MaxToolCalls {
		return nil
	}
	out := make([]*loom.ToolInfo, 0, len(all))
	for _, info := range all {
		if info == nil {
			continue
		}
		if limit := cfg.ToolCallLimits[info.Name]; limit > 0 && uses[info.Name] >= limit {
			continue
		}
		out = append(out, info)
	}
	return out
}

func reserveTool(name string, cfg Config, total *uint64, uses map[string]uint64) string {
	if cfg.MaxToolCalls > 0 && *total >= cfg.MaxToolCalls {
		return fmt.Sprintf("total tool call limit %d reached", cfg.MaxToolCalls)
	}
	if limit := cfg.ToolCallLimits[name]; limit > 0 && uses[name] >= limit {
		return fmt.Sprintf("tool %q call limit %d reached", name, limit)
	}
	*total++
	uses[name]++
	return ""
}

func rejectToolCall(ctx context.Context, w loom.Writer, call loom.ToolCall, reason string) (loom.ToolExecResult, error) {
	err := errors.New(reason)
	if writeErr := w.WriteToolCall(ctx, call.Name, call); writeErr != nil {
		return loom.ToolExecResult{}, writeErr
	}
	if writeErr := w.WriteToolResult(ctx, call.Name, loom.ToolResult{CallID: call.ID, ToolName: call.Name, Err: &loom.ItemError{Code: "tool_budget_exhausted", Message: reason}}); writeErr != nil {
		return loom.ToolExecResult{}, writeErr
	}
	return loom.ToolExecResult{Call: call, Err: err}, nil
}

func appendResponse(messages []loom.Message, response *loom.ChatResponse) []loom.Message {
	out := append([]loom.Message(nil), messages...)
	if response == nil {
		return out
	}
	return append(out, loom.Message{Role: loom.RoleAssistant, Content: response.Content, ReasoningContent: response.ReasoningContent, ToolCalls: response.ToolCalls})
}

func validateFinishReason(reason loom.FinishReason) error {
	switch reason {
	case "", loom.FinishReasonStop, loom.FinishReasonToolCalls:
		return nil
	case loom.FinishReasonContentFilter:
		return loom.ErrContentFilter
	case loom.FinishReasonLength:
		return loom.ErrOutputTruncated
	case loom.FinishReasonError:
		return errors.New("model returned error finish reason")
	default:
		return fmt.Errorf("unknown finish reason %q", reason)
	}
}
