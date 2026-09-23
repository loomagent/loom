// Package react provides a provider-neutral ReAct loop for Loom agents.
package react

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/loomagent/loom"
)

// Config controls a ReAct run.
type Config struct {
	Model    loom.ChatModel
	Tools    *loom.ToolRegistry
	Messages []loom.Message
	// SensitiveFallback retries the same model request once when the primary
	// model reports a sensitive-content rejection.
	SensitiveFallback *SensitiveFallbackConfig

	// Reasoning is sent on every model call. Its zero value defaults to enabled.
	Reasoning loom.Reasoning
	// MaxSteps is the number of tool-capable model rounds. When reached, Run
	// performs one final tool-free soft-landing call. Zero means unlimited.
	MaxSteps uint64
	// MaxToolCalls limits successful tool reservations across the run.
	MaxToolCalls uint64
	// ToolCallLimits applies per-tool limits. Missing and zero values are unlimited.
	ToolCallLimits map[string]uint64
	// MaxConsecutiveRefusals bounds batches that are refused without executing anything: a batch
	// that bundles the terminal tool with others, or one whose calls are all over budget. A run
	// that keeps being refused makes no progress, and MaxSteps=0 means unlimited, so the
	// framework bounds the loop it introduced itself; zero uses
	// defaultMaxConsecutiveRefusals, and there is deliberately no way to switch it off.
	MaxConsecutiveRefusals uint64
	// SoftLandingPrompt is appended as a system message before the final call.
	SoftLandingPrompt string
	// ToolPhaseEndedPrompt is appended as a system message once the tool phase has ended, so the
	// model knows its next reply is the answer. Empty uses a default sentence.
	ToolPhaseEndedPrompt string
	// SoftLandingReserve forces the final tool-free call when the context
	// deadline is this close. Zero disables deadline-based soft landing.
	SoftLandingReserve time.Duration
	// DeadlineSoftLandingPrompt overrides SoftLandingPrompt only when the
	// deadline reserve triggers. Empty reuses SoftLandingPrompt.
	DeadlineSoftLandingPrompt string
	// Purpose prefixes model call span names and errors.
	Purpose string

	StepPolicies       []StepPolicy
	AfterToolsPolicies []AfterToolsPolicy
	FinishPolicies     []FinishPolicy
	// TransformToolResults may compact or redact results before they are sent
	// back to the model. The original results remain visible to policies.
	TransformToolResults func([]loom.ToolExecResult) []loom.ToolExecResult
}

// SensitiveFallbackConfig configures a provider-neutral fallback for content
// filtering. Configured IDs are compared exactly only to avoid retrying an
// administrator-configured identical route; model display names are not used
// to infer route identity.
type SensitiveFallbackConfig struct {
	Model           loom.ChatModel
	PrimaryModelID  string
	FallbackModelID string
}

// State is the observable state supplied to policies.
type State struct {
	Step          uint64
	Messages      []loom.Message
	ToolInfos     []*loom.ToolInfo
	ToolCallsUsed uint64
	ToolUses      map[string]uint64
	// ToolPhaseEnded reports that a terminal tool has succeeded: no tool is available from the
	// next call on, and its content is the final answer.
	ToolPhaseEnded bool
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
	// FinalAnswerCommitted is true only when RunToFinalAnswer has streamed and
	// committed the answer. The caller must not write it a second time.
	FinalAnswerCommitted bool
	// FinalizationReason identifies how the streaming final phase was entered.
	// Values are terminal_tool, natural_finish, tool_budget, step_limit,
	// deadline, no_tools, and policy. It is empty for a buffered natural finish.
	FinalizationReason string
	Messages           []loom.Message
	SoftLanded         bool
	Steps              uint64
	// ToolPhaseEnded reports that the run ended in the final, tool-free phase.
	ToolPhaseEnded bool
	// EndedByTool names the terminal tool that ended the phase. It is empty for
	// natural finishes, policy stops, and budget boundaries.
	EndedByTool string
}

// RefusedBatchesError reports that consecutive batches were refused without executing anything, so
// the run stopped instead of asking a model again that was making no progress. The refusals were
// written as tool results before this error was returned, so the conversation stays intact for
// whatever the caller does next.
type RefusedBatchesError struct {
	Refusals uint64
	Reason   string
}

func (e *RefusedBatchesError) Error() string {
	return fmt.Sprintf("react: %d consecutive batches were refused without executing anything (last refusal: %s)", e.Refusals, e.Reason)
}

// defaultMaxConsecutiveRefusals is the bound a run uses when the caller sets none.
const defaultMaxConsecutiveRefusals = 3

// ErrToolCallInFinalPhase reports a model that asked for a tool in a round that has none. The
// final phase removes tools precisely so the answer cannot be interleaved with one, so the call
// is not executed: the caller decides whether to ask for the answer again.
var ErrToolCallInFinalPhase = loom.ErrToolCallInFinalAnswer

// Run executes a ReAct loop, streaming model output and tool events through w.
// It does not call Writer.FinalAnswer; the caller owns the surrounding Turn's
// completion semantics.
func Run(ctx context.Context, w loom.Writer, cfg Config) (*Result, error) {
	return run(ctx, w, nil, cfg)
}

// RunToFinalAnswer executes a ReAct loop and commits its final response through
// the TurnWriter. Both reasoning and content are streamed as they arrive.
// A successful terminal tool, an accepted natural finish, an after-tools stop,
// or a budget boundary enters a new tool-free model round. Natural finish text
// is retained only as a draft in model context; no tool call is fabricated.
// Choosing this entry point explicitly permits finalization without a terminal
// tool, so use Run when a terminal tool carries a mandatory business contract.
// Reasoning must be explicitly configured. Final stream errors propagate without
// automatically retrying or switching models after partial delivery.
func RunToFinalAnswer(ctx context.Context, w loom.TurnWriter, cfg Config) (*Result, error) {
	if w == nil {
		return nil, errors.New("react: TurnWriter is required")
	}
	if cfg.Reasoning.Mode == "" {
		return nil, errors.New("react: Reasoning must be explicitly configured")
	}
	return run(ctx, w, w, cfg)
}

func run(ctx context.Context, w loom.Writer, finalWriter loom.TurnWriter, cfg Config) (*Result, error) {
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
	if finalWriter != nil {
		provider, modelName := loom.SplitModelName(cfg.Model.Name())
		if _, err := loom.ResolveModelReasoning(provider, modelName, cfg.Model.Capabilities(), reasoning); err != nil {
			return nil, err
		}
	}
	if reasoning.Mode == "" {
		reasoning.Mode = loom.ReasoningModeEnabled
	}

	allTools, err := cfg.Tools.InfoList(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: list tools: %w", purpose, err)
	}
	terminalTools := terminalToolNames(allTools)
	maxRefusals := cfg.MaxConsecutiveRefusals
	if maxRefusals == 0 {
		maxRefusals = defaultMaxConsecutiveRefusals
	}
	var refusedInARow uint64
	var toolPhaseEnded bool
	var endedByTool string
	var finalizationReason string
	msgs := append([]loom.Message(nil), cfg.Messages...)
	uses := make(map[string]uint64, len(cfg.ToolCallLimits))
	var totalUses uint64

	for step := uint64(0); ; step++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		state := snapshotState(step, msgs, allTools, totalUses, uses, toolPhaseEnded)
		plan := StepPlan{
			Messages: append([]loom.Message(nil), msgs...),
			Tools:    availableTools(allTools, cfg, totalUses, uses),
		}
		loopLimitReached := cfg.MaxSteps > 0 && step >= cfg.MaxSteps
		deadlineNear := false
		if deadline, ok := ctx.Deadline(); ok && cfg.SoftLandingReserve > 0 {
			deadlineNear = time.Until(deadline) <= cfg.SoftLandingReserve
		}
		var finalPrompt string
		switch {
		case toolPhaseEnded || finalizationReason != "":
			// The tool phase is over: every later request carries no tools, so its content is
			// the answer by construction, and the phase cannot be reopened.
			plan.IsFinalStep = true
			plan.Tools = nil
			plan.ToolChoice = &loom.ToolChoice{Mode: loom.ToolChoiceNone}
			prompt := strings.TrimSpace(cfg.ToolPhaseEndedPrompt)
			if prompt == "" {
				prompt = defaultToolPhaseEndedPrompt
			}
			finalPrompt = prompt
		case loopLimitReached || deadlineNear:
			finalizationReason = "step_limit"
			if deadlineNear {
				finalizationReason = "deadline"
			}
			plan.IsFinalStep = true
			plan.Tools = nil
			plan.ToolChoice = &loom.ToolChoice{Mode: loom.ToolChoiceNone}
			prompt := cfg.SoftLandingPrompt
			if deadlineNear && strings.TrimSpace(cfg.DeadlineSoftLandingPrompt) != "" {
				prompt = cfg.DeadlineSoftLandingPrompt
			}
			if finalWriter != nil && strings.TrimSpace(prompt) == "" {
				prompt = defaultToolPhaseEndedPrompt
			}
			if prompt = strings.TrimSpace(prompt); prompt != "" {
				finalPrompt = prompt
			}
		}
		if finalWriter != nil && !plan.IsFinalStep && (len(allTools) == 0 || researchBudgetExhausted(allTools, plan.Tools)) {
			finalizationReason = "tool_budget"
			if len(allTools) == 0 {
				finalizationReason = "no_tools"
			}
			plan.IsFinalStep = true
			plan.Tools = nil
			plan.ToolChoice = &loom.ToolChoice{Mode: loom.ToolChoiceNone}
			prompt := strings.TrimSpace(cfg.SoftLandingPrompt)
			if len(allTools) == 0 {
				prompt = strings.TrimSpace(cfg.ToolPhaseEndedPrompt)
			}
			if prompt == "" {
				prompt = defaultToolPhaseEndedPrompt
			}
			finalPrompt = prompt
		}
		if finalWriter == nil && finalPrompt != "" {
			plan.Messages = append(plan.Messages, loom.Message{Role: loom.RoleSystem, Content: finalPrompt})
		}
		finalPlanned := plan.IsFinalStep
		for _, policy := range cfg.StepPolicies {
			if policy != nil {
				if err := policy.PrepareStep(ctx, state, &plan); err != nil {
					return nil, fmt.Errorf("%s: step %d policy: %w", purpose, step+1, err)
				}
			}
		}
		// Policies may transform context but cannot reopen a completed phase or
		// leave tools attached to a round they designate as final.
		if finalPlanned || plan.IsFinalStep {
			plan.IsFinalStep = true
			plan.Tools = nil
			plan.ToolChoice = &loom.ToolChoice{Mode: loom.ToolChoiceNone}
			if finalizationReason == "" {
				finalizationReason = "policy"
			}
			if finalWriter != nil {
				if finalPrompt == "" {
					finalPrompt = defaultToolPhaseEndedPrompt
				}
				plan.Messages = append(plan.Messages, loom.Message{Role: loom.RoleSystem, Content: finalPrompt})
			}
		}
		if len(plan.Tools) == 0 && plan.ToolChoice == nil {
			plan.ToolChoice = &loom.ToolChoice{Mode: loom.ToolChoiceNone}
		}
		state.Messages = append([]loom.Message(nil), plan.Messages...)

		stepPurpose := fmt.Sprintf("%s.step_%d", purpose, step+1)
		request := loom.ChatRequest{
			Messages:   plan.Messages,
			Tools:      plan.Tools,
			ToolChoice: plan.ToolChoice,
			Reasoning:  reasoning,
		}
		var response *loom.ChatResponse
		if plan.IsFinalStep && finalWriter != nil {
			response, err = loom.StreamLLMToFinalAnswer(ctx, finalWriter, stepPurpose, cfg.Model, request)
		} else {
			response, err = streamWithSensitiveFallback(ctx, w, stepPurpose, cfg.Model, request, cfg.SensitiveFallback)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: step %d model: %w", purpose, step+1, err)
		}
		if err := validateFinishReason(response.FinishReason); err != nil {
			return nil, fmt.Errorf("%s: step %d: %w", purpose, step+1, err)
		}
		if plan.IsFinalStep {
			// A final phase has no tools to call: a tool call here is not executed, and the
			// caller decides whether to ask for the answer again.
			if len(response.ToolCalls) > 0 {
				return nil, fmt.Errorf("%s: step %d: %w", purpose, step+1, ErrToolCallInFinalPhase)
			}
			return &Result{
				FinalContent:         response.Content,
				FinalAnswerCommitted: finalWriter != nil,
				FinalizationReason:   finalizationReason,
				Messages:             appendResponse(plan.Messages, response),
				SoftLanded:           (finalWriter == nil && !toolPhaseEnded) || finalizationReason == "step_limit" || finalizationReason == "deadline" || finalizationReason == "tool_budget",
				Steps:                step + 1,
				ToolPhaseEnded:       true,
				EndedByTool:          endedByTool,
			}, nil
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
			if finalWriter != nil {
				if response.FinishReason != loom.FinishReasonStop {
					return nil, fmt.Errorf("%s: natural finish requires a normal stop, got %q", purpose, response.FinishReason)
				}
				if strings.TrimSpace(response.Content) == "" {
					return nil, loom.ErrEmptyFinalAnswer
				}
				msgs = appendResponse(plan.Messages, response)
				msgs = append(msgs, loom.Message{Role: loom.RoleSystem, Content: "The preceding answer is an undelivered draft. The next response is the final answer for the user."})
				finalizationReason = "natural_finish"
				continue
			}
			return &Result{FinalContent: response.Content, Messages: appendResponse(plan.Messages, response), Steps: step + 1}, nil
		}

		results := make([]loom.ToolExecResult, 0, len(response.ToolCalls))
		executed, lastRefusal := false, ""
		if reason := mixedTerminalBatch(response.ToolCalls, terminalTools); reason != "" {
			lastRefusal = "terminal_tool_not_alone"
			// Nothing in this batch runs; every call still gets a result, because the next
			// request is invalid without one per call id.
			for _, call := range response.ToolCalls {
				result, err := rejectToolCall(ctx, w, call, "terminal_tool_not_alone", reason)
				if err != nil {
					return nil, fmt.Errorf("%s: step %d reject batch: %w", purpose, step+1, err)
				}
				results = append(results, result)
			}
		} else {
			for _, call := range response.ToolCalls {
				if reason := reserveTool(call.Name, cfg, &totalUses, uses, terminalTools); reason != "" {
					lastRefusal = "tool_budget_exhausted"
					result, err := rejectToolCall(ctx, w, call, "tool_budget_exhausted", reason)
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
				executed = true
				for _, result := range one {
					// Only success ends the phase; a failure comes back as a tool result and the
					// loop continues.
					if result.Err == nil && terminalTools[result.Call.Name] {
						toolPhaseEnded = true
						endedByTool = result.Call.Name
						finalizationReason = "terminal_tool"
					}
				}
				results = append(results, one...)
			}
		}
		promptResults := results
		if cfg.TransformToolResults != nil {
			promptResults = cfg.TransformToolResults(append([]loom.ToolExecResult(nil), results...))
		}
		msgs = loom.AppendAssistantTurn(plan.Messages, response, promptResults)

		state = snapshotState(step, msgs, allTools, totalUses, uses, toolPhaseEnded)
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
				if finalWriter != nil {
					if decision.FinalContent != "" {
						msgs = append(msgs, loom.Message{Role: loom.RoleAssistant, Content: decision.FinalContent})
					}
					if finalizationReason == "" {
						finalizationReason = "policy"
					}
					break
				}
				return &Result{FinalContent: decision.FinalContent, Messages: msgs, Steps: step + 1}, nil
			}
		}
		// A batch that ran nothing is not progress: the model may be asking for tools it cannot
		// have, or bundling the terminal tool with others, and the loop this framework added
		// would otherwise spin until MaxSteps, which is unlimited when the caller sets none.
		if finalWriter != nil && finalizationReason != "" {
			continue
		}
		if executed {
			refusedInARow = 0
			continue
		}
		refusedInARow++
		if refusedInARow >= maxRefusals {
			return nil, fmt.Errorf("%s: step %d: %w", purpose, step+1, &RefusedBatchesError{Refusals: refusedInARow, Reason: lastRefusal})
		}
	}
}

// A signal-only registry still gets a tool-capable round. Only exhausted
// research tools trigger this boundary; terminal tools themselves are exempt.
func researchBudgetExhausted(all, available []*loom.ToolInfo) bool {
	hadResearch := false
	for _, info := range all {
		if info != nil && !info.EndsToolPhase {
			hadResearch = true
			break
		}
	}
	if !hadResearch {
		return false
	}
	for _, info := range available {
		if info != nil && !info.EndsToolPhase {
			return false
		}
	}
	return true
}

func streamWithSensitiveFallback(
	ctx context.Context,
	w loom.Writer,
	purpose string,
	primary loom.ChatModel,
	request loom.ChatRequest,
	fallback *SensitiveFallbackConfig,
) (*loom.ChatResponse, error) {
	response, err := loom.StreamLLMToStep(ctx, w, purpose, primary, request)
	if err != nil {
		if !isSensitiveModelError(err) || fallback == nil || fallback.Model == nil {
			return nil, err
		}
		if sameConfiguredModelID(fallback) {
			if noteErr := writeFallbackNote(ctx, w, fallback, primary, purpose, sensitiveErrorClass(err), true); noteErr != nil {
				return nil, noteErr
			}
			return nil, err
		}
		if noteErr := writeFallbackNote(ctx, w, fallback, primary, purpose, sensitiveErrorClass(err), false); noteErr != nil {
			return nil, noteErr
		}
		return loom.StreamLLMToStep(ctx, w, purpose+".sensitive_fallback", fallback.Model, request)
	}
	if response == nil || response.FinishReason != loom.FinishReasonContentFilter || fallback == nil || fallback.Model == nil {
		return response, nil
	}
	if sameConfiguredModelID(fallback) {
		if noteErr := writeFallbackNote(ctx, w, fallback, primary, purpose, "content_filter", true); noteErr != nil {
			return nil, noteErr
		}
		return response, nil
	}
	if noteErr := writeFallbackNote(ctx, w, fallback, primary, purpose, "content_filter", false); noteErr != nil {
		return nil, noteErr
	}
	return loom.StreamLLMToStep(ctx, w, purpose+".sensitive_fallback", fallback.Model, request)
}

func isSensitiveModelError(err error) bool {
	return errors.Is(err, loom.ErrSensitiveContentRisk) || errors.Is(err, loom.ErrContentFilter)
}

func sensitiveErrorClass(err error) string {
	if errors.Is(err, loom.ErrSensitiveContentRisk) {
		return "sensitive_content_risk"
	}
	return "content_filter"
}

func sameConfiguredModelID(config *SensitiveFallbackConfig) bool {
	if config == nil {
		return false
	}
	primaryID := strings.TrimSpace(config.PrimaryModelID)
	fallbackID := strings.TrimSpace(config.FallbackModelID)
	return primaryID != "" && fallbackID != "" && primaryID == fallbackID
}

func writeFallbackNote(
	ctx context.Context,
	w loom.Writer,
	config *SensitiveFallbackConfig,
	primary loom.ChatModel,
	purpose string,
	errorClass string,
	skipped bool,
) error {
	decision := "retry"
	message := "Primary model triggered sensitive-content filtering; retrying the same request with the configured sensitive fallback model."
	if skipped {
		decision = "skip_same_model_id"
		message = "Primary model triggered sensitive-content filtering, but the configured fallback uses the same model ID; skipping an ineffective retry."
	}
	primaryName := ""
	if primary != nil {
		primaryName = primary.Name()
	}
	fallbackName := ""
	if config != nil && config.Model != nil {
		fallbackName = config.Model.Name()
	}
	note := fmt.Sprintf(
		"%s decision=%s error_class=%s primary_model_id=%s fallback_model_id=%s primary_model=%s fallback_model=%s purpose=%s",
		message,
		decision,
		errorClass,
		config.PrimaryModelID,
		config.FallbackModelID,
		primaryName,
		fallbackName,
		purpose,
	)
	if err := w.WriteReasoning(ctx, "sensitive_fallback", note); err != nil {
		return fmt.Errorf("react: write sensitive fallback decision: %w", err)
	}
	return nil
}

func snapshotState(step uint64, messages []loom.Message, tools []*loom.ToolInfo, total uint64, uses map[string]uint64, phaseEnded bool) State {
	useCopy := make(map[string]uint64, len(uses))
	maps.Copy(useCopy, uses)
	return State{Step: step, Messages: append([]loom.Message(nil), messages...), ToolInfos: append([]*loom.ToolInfo(nil), tools...), ToolCallsUsed: total, ToolUses: useCopy, ToolPhaseEnded: phaseEnded}
}

// terminalToolNames returns the tools marked as ending the tool phase. The marker comes from the
// registry, never from a name a model printed.
func terminalToolNames(tools []*loom.ToolInfo) map[string]bool {
	names := make(map[string]bool)
	for _, info := range tools {
		if info != nil && info.EndsToolPhase {
			names[info.Name] = true
		}
	}
	return names
}

// mixedTerminalBatch reports why a batch that contains a terminal tool together with anything else
// is refused, and returns an empty string when the batch may run. The rule is a call count: a batch
// containing a terminal tool has exactly one call. Refusing the whole batch, rather than only the
// terminal call, is what makes "nothing happened" true: a side-effecting tool in the same batch
// would otherwise run, be read by the model as part of a failed batch, and run again.
func mixedTerminalBatch(calls []loom.ToolCall, terminalTools map[string]bool) string {
	if len(calls) < 2 {
		return ""
	}
	var terminals, others []string
	for _, call := range calls {
		if terminalTools[call.Name] {
			terminals = append(terminals, call.Name)
			continue
		}
		others = append(others, call.Name)
	}
	if len(terminals) == 0 {
		return ""
	}
	const skipped = "Every call in this batch was skipped and the tool phase is still open."
	if len(terminals) > 1 {
		return fmt.Sprintf("%s were called in one batch. %s Call exactly one of them, on its own, once you need no tool.",
			strings.Join(terminals, " and "), skipped)
	}
	return fmt.Sprintf("%s must be called on its own, and this batch also called %s. %s Re-send the normal calls you still need first, then call %s alone once you need no tool. Do not treat anything in this batch as done.",
		terminals[0], strings.Join(others, ", "), skipped, terminals[0])
}

// defaultToolPhaseEndedPrompt says what changed once the tool phase ended: tools are gone, so the
// next reply can only be the answer.
const defaultToolPhaseEndedPrompt = "The tool phase is over: no tool is available any more. Write the final answer now."

func availableTools(all []*loom.ToolInfo, cfg Config, total uint64, uses map[string]uint64) []*loom.ToolInfo {
	outOfBudget := cfg.MaxToolCalls > 0 && total >= cfg.MaxToolCalls
	out := make([]*loom.ToolInfo, 0, len(all))
	for _, info := range all {
		if info == nil {
			continue
		}
		// A terminal tool is not a research tool: it is the only way the phase can end, so no
		// budget withholds it.
		if info.EndsToolPhase {
			out = append(out, info)
			continue
		}
		if outOfBudget {
			continue
		}
		if limit := cfg.ToolCallLimits[info.Name]; limit > 0 && uses[info.Name] >= limit {
			continue
		}
		out = append(out, info)
	}
	return out
}

func reserveTool(name string, cfg Config, total *uint64, uses map[string]uint64, terminalTools map[string]bool) string {
	// A terminal tool is not a research tool: no budget withholds it, or the phase could never
	// end.
	if terminalTools[name] {
		return ""
	}
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

func rejectToolCall(ctx context.Context, w loom.Writer, call loom.ToolCall, code, reason string) (loom.ToolExecResult, error) {
	err := errors.New(reason)
	if writeErr := w.WriteToolCall(ctx, call.Name, call); writeErr != nil {
		return loom.ToolExecResult{}, writeErr
	}
	if writeErr := w.WriteToolResult(ctx, call.Name, loom.ToolResult{CallID: call.ID, ToolName: call.Name, Err: &loom.ItemError{Code: code, Message: reason}}); writeErr != nil {
		return loom.ToolExecResult{}, writeErr
	}
	return loom.ToolExecResult{Call: call, Err: err}, nil
}

func appendResponse(messages []loom.Message, response *loom.ChatResponse) []loom.Message {
	out := append([]loom.Message(nil), messages...)
	if response == nil {
		return out
	}
	return append(out, loom.Message{
		Role:             loom.RoleAssistant,
		Content:          response.Content,
		ReasoningContent: response.ReasoningContent,
		// A provider that returns structured reasoning wants the same blocks back on the turn
		// they belong to, which is why they travel with the message.
		ReasoningDetails: response.ReasoningDetails,
		ToolCalls:        response.ToolCalls,
	})
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
