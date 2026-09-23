package react

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/loomagent/loom"
)

// finalPhaseModel is an in-process provider fake. Research uses complete protocol
// responses; the final stream is driven one frame at a time by the test.
type finalPhaseModel struct {
	scriptedModel
	frames        chan *loom.Chunk
	finalRequests []loom.ChatRequest
}

func (m *finalPhaseModel) Stream(ctx context.Context, req loom.ChatRequest) (loom.Stream, error) {
	if len(req.Tools) != 0 {
		return m.scriptedModel.Stream(ctx, req)
	}
	m.finalRequests = append(m.finalRequests, req)
	return &finalPhaseStream{ctx: ctx, frames: m.frames}, nil
}

type finalPhaseStream struct {
	ctx    context.Context
	frames <-chan *loom.Chunk
}

func (s *finalPhaseStream) Recv() (*loom.Chunk, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case frame, ok := <-s.frames:
		if !ok {
			return nil, io.EOF
		}
		return frame, nil
	}
}

func (*finalPhaseStream) Close() error { return nil }

func TestUserReceivesFinalReasoningAndContentBeforeModelFinishes(t *testing.T) {
	for _, entry := range []string{"terminal_tool", "natural_finish"} {
		t.Run("user_"+entry, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given a tool-using agent and a final provider stream we can pause.
				sink := loom.NewMemorySink()
				calls := 0
				first := &loom.ChatResponse{Content: "undelivered draft", FinishReason: loom.FinishReasonStop}
				if entry == "terminal_tool" {
					first = &loom.ChatResponse{ToolCalls: []loom.ToolCall{{ID: "finish", Name: "finalize_answer"}}, FinishReason: loom.FinishReasonToolCalls}
				}
				model := &finalPhaseModel{frames: make(chan *loom.Chunk)}
				model.responses = []*loom.ChatResponse{first}
				var turn *loom.Turn
				var result *Result
				var runErr error
				done := make(chan struct{})
				go func() {
					defer close(done)
					turn, runErr = loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
						var err error
						result, err = RunToFinalAnswer(ctx, w, Config{Model: model, Tools: loom.NewToolRegistry(terminalTool(&calls)), Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled}, ToolPhaseEndedPrompt: "Write the final answer now.", MaxTokens: new(1024),
							CallModel: func(ctx context.Context, w loom.Writer, _ State, purpose string, model loom.ChatModel, req loom.ChatRequest) (*loom.ChatResponse, error) {
								if len(req.Tools) == 0 {
									t.Error("host research caller received final delivery")
								}
								return loom.StreamLLMToStep(ctx, w, purpose, model, req)
							},
						})
						return err
					}, loom.RunOptions{ConversationID: "streaming-final", Sinks: []loom.Sink{sink}, StrictSink: true})
				}()
				// When each frame arrives, its delta must reach the sink before EOF.
				for i, frame := range []*loom.Chunk{
					{ReasoningContentDelta: "consider"},
					{ReasoningContentDelta: " evidence"},
					{ContentDelta: "Hello"},
					{ContentDelta: " world"},
				} {
					model.frames <- frame
					synctest.Wait()
					deltas := sink.DeltaEvents()
					if len(deltas) != i+1 {
						t.Fatalf("user must see each frame immediately: got %d deltas after frame %d", len(deltas), i+1)
					}
					select {
					case <-done:
						t.Fatal("turn finished before stream EOF")
					default:
					}
					for _, event := range sink.FinishedEvents() {
						if event.Item.Kind == loom.ItemKindFinalAnswer {
							t.Fatal("answer committed before stream EOF")
						}
					}
				}
				model.frames <- &loom.Chunk{FinishReason: loom.FinishReasonStop, Usage: &loom.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8}}
				close(model.frames)
				<-done
				// Then one completed answer follows optional reasoning; no draft or fake call leaks.
				if runErr != nil || turn.Status != loom.TurnStatusCompleted {
					t.Fatalf("turn=%+v err=%v", turn, runErr)
				}
				if result.FinalContent != "Hello world" || !result.FinalAnswerCommitted {
					t.Fatalf("result=%+v", result)
				}
				if len(model.finalRequests) != 1 || model.finalRequests[0].Reasoning.Mode != loom.ReasoningModeEnabled || model.finalRequests[0].MaxTokens == nil || *model.finalRequests[0].MaxTokens != 1024 {
					t.Fatal("final phase changed reasoning configuration")
				}
				if model.finalRequests[0].Messages[len(model.finalRequests[0].Messages)-1].Content != "Write the final answer now." {
					t.Fatal("final instruction missing")
				}
				var kinds []loom.ItemKind
				for _, item := range turn.Items {
					if item.Kind == loom.ItemKindReasoning || item.Kind == loom.ItemKindFinalAnswer {
						kinds = append(kinds, item.Kind)
					}
					if item.Text == "undelivered draft" {
						t.Fatal("draft was delivered")
					}
				}
				if !slices.Equal(kinds, []loom.ItemKind{loom.ItemKindReasoning, loom.ItemKindFinalAnswer}) {
					t.Fatalf("output order=%v", kinds)
				}
				if entry == "natural_finish" && calls != 0 {
					t.Fatal("natural completion fabricated a tool call")
				}
				if turn.Usage.TotalTokens != 8 {
					t.Fatalf("usage lost: %+v", turn.Usage)
				}
			})
		})
	}
}

func TestUserBudgetAndPolicyStopsStillStreamAFinalAnswer(t *testing.T) {
	for _, route := range []string{"tool_budget", "per_tool_budget", "step_limit", "deadline", "policy", "terminal_policy_cannot_reopen", "finish_policy"} {
		t.Run("user_"+route, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given a research round that reaches a budget or policy boundary.
				calls := 0
				lookup := loom.NewArgsTool(loom.MustArgsContract("lookup"), "Look up a fact", func(context.Context, loom.Args) (string, error) { return "evidence", nil })
				first := &loom.ChatResponse{ToolCalls: []loom.ToolCall{{ID: "lookup", Name: "lookup", Arguments: `{}`}}, FinishReason: loom.FinishReasonToolCalls}
				model := &finalPhaseModel{frames: make(chan *loom.Chunk, 3)}
				model.responses = []*loom.ChatResponse{first}
				model.frames <- &loom.Chunk{ReasoningContentDelta: "use available evidence"}
				model.frames <- &loom.Chunk{ContentDelta: "answer"}
				model.frames <- &loom.Chunk{FinishReason: loom.FinishReasonStop}
				close(model.frames)
				cfg := Config{Model: model, Tools: loom.NewToolRegistry(lookup, terminalTool(&calls)), Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled}, ToolPhaseEndedPrompt: "Final response only."}
				ctx := t.Context()
				wantReason := route
				switch route {
				case "tool_budget":
					cfg.MaxToolCalls = 1
				case "per_tool_budget":
					cfg.ToolCallLimits = map[string]uint64{"lookup": 1}
					wantReason = "tool_budget"
				case "step_limit":
					cfg.MaxSteps = 1
				case "deadline":
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Minute)
					defer cancel()
					cfg.SoftLandingReserve = 2 * time.Minute
				case "policy":
					cfg.AfterToolsPolicies = []AfterToolsPolicy{AfterToolsPolicyFunc(func(context.Context, loom.Writer, State, []loom.ToolExecResult) (AfterToolsDecision, error) {
						return AfterToolsDecision{Stop: true, FinalContent: "policy draft"}, nil
					})}
				case "terminal_policy_cannot_reopen":
					first.ToolCalls = []loom.ToolCall{{ID: "finish", Name: "finalize_answer"}}
					wantReason = "terminal_tool"
					cfg.StepPolicies = []StepPolicy{StepPolicyFunc(func(_ context.Context, state State, plan *StepPlan) error {
						if state.ToolPhaseEnded {
							plan.Tools = state.ToolInfos
							plan.IsFinalStep = false
							plan.Messages = nil
							plan.ToolChoice = &loom.ToolChoice{Mode: loom.ToolChoiceRequired}
						}
						return nil
					})}
				case "finish_policy":
					first.Content = "draft"
					first.ToolCalls = nil
					first.FinishReason = loom.FinishReasonStop
					model.responses = append(model.responses, &loom.ChatResponse{ToolCalls: []loom.ToolCall{{ID: "finish", Name: "finalize_answer"}}, FinishReason: loom.FinishReasonToolCalls})
					wantReason = "terminal_tool"
					cfg.FinishPolicies = []FinishPolicy{FinishPolicyFunc(func(context.Context, State, *loom.ChatResponse) (FinishDecision, error) {
						return FinishDecision{Continue: true, Instruction: "Use the terminal signal."}, nil
					})}
				}
				// When the loop finishes, every route must deliver the final model's text.
				var result *Result
				turn, err := loom.Run(ctx, func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
					var err error
					result, err = RunToFinalAnswer(ctx, w, cfg)
					return err
				}, loom.RunOptions{ConversationID: route})
				// Then no policy can reopen tools or commit its buffered draft instead.
				if err != nil || turn.Status != loom.TurnStatusCompleted {
					t.Fatalf("turn=%+v err=%v", turn, err)
				}
				if !result.FinalAnswerCommitted || result.FinalContent != "answer" || result.FinalizationReason != wantReason {
					t.Fatalf("result=%+v", result)
				}
				request := model.finalRequests[0]
				if len(request.Tools) != 0 || request.ToolChoice == nil || request.ToolChoice.Mode != loom.ToolChoiceNone {
					t.Fatalf("final request tools=%v choice=%v", request.Tools, request.ToolChoice)
				}
				if route == "terminal_policy_cannot_reopen" && request.Messages[len(request.Messages)-1].Content != "Final response only." {
					t.Fatal("policy removed the final instruction")
				}
			})
		})
	}
}

func TestUserInvalidFinalizationConfigOrEmptyDraftFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given a missing reasoning choice or an empty natural response, when run,
		// then no successful final answer or synthetic terminal call is produced.
		if _, err := RunToFinalAnswer(t.Context(), nil, Config{}); err == nil {
			t.Fatal("nil writer accepted")
		}
		calls := 0
		model := &scriptedModel{responses: []*loom.ChatResponse{{FinishReason: loom.FinishReasonStop}}}
		turn, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
			cfg := Config{Model: model, Tools: loom.NewToolRegistry(terminalTool(&calls))}
			if _, err := RunToFinalAnswer(ctx, w, cfg); err == nil {
				t.Error("implicit reasoning accepted")
			}
			cfg.Reasoning = loom.Reasoning{Mode: "invalid"}
			if _, err := RunToFinalAnswer(ctx, w, cfg); err == nil {
				t.Error("invalid reasoning accepted")
			}
			cfg.Reasoning = loom.Reasoning{Mode: loom.ReasoningModeDisabled}
			_, err := RunToFinalAnswer(ctx, w, cfg)
			return err
		}, loom.RunOptions{ConversationID: "invalid-finalization"})
		if !errors.Is(err, loom.ErrEmptyFinalAnswer) || turn.Status != loom.TurnStatusFailed || calls != 0 {
			t.Fatalf("turn=%+v err=%v", turn, err)
		}
	})
}

func TestUserDoesNotReceiveSuccessfulAnswerFromInvalidFinalStream(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame *loom.Chunk
		want  error
	}{
		{"truncated", &loom.Chunk{FinishReason: loom.FinishReasonLength}, loom.ErrOutputTruncated},
		{"filtered", &loom.Chunk{FinishReason: loom.FinishReasonContentFilter}, loom.ErrContentFilter},
		{"tool_call", &loom.Chunk{ToolCallDeltas: []loom.ToolCallDelta{{ID: "bad", Name: "lookup"}}}, ErrToolCallInFinalPhase},
	} {
		t.Run("user_"+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given a final response with a partial answer followed by a protocol failure.
				model := &finalPhaseModel{frames: make(chan *loom.Chunk, 2)}
				model.frames <- &loom.Chunk{ContentDelta: "partial"}
				model.frames <- tc.frame
				close(model.frames)
				turn, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
					_, err := RunToFinalAnswer(ctx, w, Config{Model: model, Tools: loom.NewToolRegistry(), Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}})
					return err
				}, loom.RunOptions{ConversationID: "failed-final"})
				// Then the partial answer stays failed and cannot seal a successful turn.
				if !errors.Is(err, tc.want) || turn.Status != loom.TurnStatusFailed {
					t.Fatalf("turn=%+v err=%v", turn, err)
				}
				for _, item := range turn.Items {
					if item.Kind == loom.ItemKindFinalAnswer && item.Status == loom.ItemStatusCompleted {
						t.Fatal("partial answer was committed")
					}
				}
			})
		})
	}
}
