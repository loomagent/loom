package react

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/loomagent/loom"
)

type scriptedModel struct {
	name         string
	capabilities loom.ModelCapabilities
	responses    []*loom.ChatResponse
	errs         []error
	requests     []loom.ChatRequest
}

type modelContextKey struct{}

type contextCheckingModel struct {
	loom.ChatModel
	t     *testing.T
	calls []ModelCallInfo
}

func (m *contextCheckingModel) Stream(ctx context.Context, req loom.ChatRequest) (loom.Stream, error) {
	info, ok := ctx.Value(modelContextKey{}).(ModelCallInfo)
	if !ok {
		m.t.Fatal("provider did not receive the prepared call context")
	}
	if info.IsFinalStep && len(req.Tools) != 0 {
		m.t.Fatal("final context tagged before tools were removed")
	}
	m.calls = append(m.calls, info)
	return m.ChatModel.Stream(ctx, req)
}

func TestModelCallContextCoversDeliveryWithoutLeakingToPolicies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		reason   string
		buffered bool
		custom   bool
	}{
		{name: "terminal", reason: "terminal_tool"},
		{name: "natural finish", reason: "natural_finish"},
		{name: "step limit", reason: "step_limit"},
		{name: "policy changes phase", reason: "policy"},
		{name: "no tools", reason: "no_tools"},
		{name: "custom research caller", reason: "terminal_tool", custom: true},
		{name: "buffered", buffered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first := &loom.ChatResponse{ToolCalls: []loom.ToolCall{{ID: "finish", Name: "finalize_answer", Arguments: `{}`}}, FinishReason: loom.FinishReasonToolCalls}
			if tc.reason == "natural_finish" || tc.buffered {
				first = &loom.ChatResponse{Content: "draft", FinishReason: loom.FinishReasonStop}
			}
			responses := []*loom.ChatResponse{first, {Content: "answer", FinishReason: loom.FinishReasonStop}}
			if tc.reason == "policy" || tc.reason == "no_tools" {
				responses = responses[1:]
			}
			model := &contextCheckingModel{t: t, ChatModel: &scriptedModel{responses: responses}}
			calls := 0
			cfg := Config{
				Model: model, Tools: loom.NewToolRegistry(terminalTool(&calls)),
				Reasoning:            loom.Reasoning{Mode: loom.ReasoningModeDisabled},
				Messages:             []loom.Message{{Role: loom.RoleUser, Content: "task"}},
				ToolPhaseEndedPrompt: "settled final instruction", Purpose: "test",
				PrepareModelCallContext: func(ctx context.Context, info ModelCallInfo) (context.Context, error) {
					return context.WithValue(ctx, modelContextKey{}, info), nil
				},
				StepPolicies: []StepPolicy{StepPolicyFunc(func(ctx context.Context, _ State, plan *StepPlan) error {
					if ctx.Value(modelContextKey{}) != nil {
						t.Fatal("request context leaked into a policy or later iteration")
					}
					if tc.reason == "policy" {
						plan.IsFinalStep = true
					}
					return nil
				})},
			}
			if tc.reason == "no_tools" {
				cfg.Tools = loom.NewToolRegistry()
			}
			if tc.reason == "step_limit" {
				cfg.MaxSteps = 1
				cfg.Tools = loom.NewToolRegistry(loom.NewArgsTool(loom.MustArgsContract("finalize_answer"), "lookup", func(context.Context, loom.Args) (string, error) { return "evidence", nil }))
			}
			if tc.custom {
				cfg.CallModel = func(ctx context.Context, w loom.Writer, _ State, purpose string, model loom.ChatModel, req loom.ChatRequest) (*loom.ChatResponse, error) {
					return loom.StreamLLMToStep(ctx, w, purpose, model, req)
				}
			}
			_, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
				if tc.buffered {
					result, err := Run(ctx, w, cfg)
					if err != nil {
						return err
					}
					return w.FinalAnswer(ctx, result.FinalContent)
				}
				_, err := RunToFinalAnswer(ctx, w, cfg)
				return err
			}, loom.RunOptions{ConversationID: tc.name})
			if err != nil {
				t.Fatal(err)
			}
			last := model.calls[len(model.calls)-1]
			if last.IsFinalStep == tc.buffered || last.FinalizationReason != tc.reason {
				t.Fatalf("final call metadata = %+v", last)
			}
			for i, call := range model.calls {
				if call.Step != uint64(i) || call.Purpose != fmt.Sprintf("test.step_%d", i+1) {
					t.Fatalf("call %d = %+v", i, call)
				}
			}
		})
	}
}

func TestModelCallContextAdmissionFailurePreventsProviderCall(t *testing.T) {
	t.Parallel()
	denied := errors.New("admission denied")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "rejected", err: denied},
		{name: "nil context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			model := &scriptedModel{}
			_, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
				_, err := RunToFinalAnswer(ctx, w, Config{
					Model: model, Messages: []loom.Message{{Role: loom.RoleUser, Content: "task"}},
					Tools:                   loom.NewToolRegistry(),
					Reasoning:               loom.Reasoning{Mode: loom.ReasoningModeDisabled},
					PrepareModelCallContext: func(context.Context, ModelCallInfo) (context.Context, error) { return nil, tc.err },
				})
				return err
			}, loom.RunOptions{ConversationID: tc.name})
			if err == nil || tc.err != nil && !errors.Is(err, tc.err) || len(model.requests) != 0 {
				t.Fatalf("error=%v provider calls=%d", err, len(model.requests))
			}
		})
	}
}

func (m *scriptedModel) Name() string {
	if m.name != "" {
		return m.name
	}
	return "test/model"
}
func (m *scriptedModel) Capabilities() loom.ModelCapabilities {
	return m.capabilities
}
func (m *scriptedModel) Chat(context.Context, loom.ChatRequest) (*loom.ChatResponse, error) {
	return nil, errors.New("unexpected Chat call")
}
func (m *scriptedModel) Stream(_ context.Context, req loom.ChatRequest) (loom.Stream, error) {
	m.requests = append(m.requests, req)
	if len(m.errs) > 0 {
		err := m.errs[0]
		m.errs = m.errs[1:]
		return nil, err
	}
	if len(m.responses) == 0 {
		return nil, errors.New("no scripted response")
	}
	response := m.responses[0]
	m.responses = m.responses[1:]
	chunk := &loom.Chunk{
		ContentDelta:          response.Content,
		ReasoningContentDelta: response.ReasoningContent,
		FinishReason:          response.FinishReason,
		Usage:                 &response.Usage,
		Model:                 response.Model,
	}
	for i, call := range response.ToolCalls {
		chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, loom.ToolCallDelta{Index: i, ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	return &sliceStream{chunks: []*loom.Chunk{chunk}}, nil
}

func TestRunSensitiveFallbackRetriesRejectedRequest(t *testing.T) {
	primary := &scriptedModel{name: "primary", errs: []error{fmt.Errorf("provider rejected: %w", loom.ErrSensitiveContentRisk)}}
	fallback := &scriptedModel{name: "fallback", responses: []*loom.ChatResponse{{Content: "safe answer", FinishReason: loom.FinishReasonStop}}}

	var result *Result
	_, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var runErr error
		result, runErr = Run(ctx, w, Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "test task"}},
			Model: primary,
			Tools: loom.NewToolRegistry(),
			SensitiveFallback: &SensitiveFallbackConfig{
				Model: fallback, PrimaryModelID: "primary-route", FallbackModelID: "fallback-route",
			},
		})
		if runErr != nil {
			return runErr
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "sensitive-error"})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalContent != "safe answer" || len(primary.requests) != 1 || len(fallback.requests) != 1 {
		t.Fatalf("result=%+v primary calls=%d fallback calls=%d", result, len(primary.requests), len(fallback.requests))
	}
	if !reflect.DeepEqual(fallback.requests[0], primary.requests[0]) {
		t.Fatalf("fallback did not retry the same request: primary=%+v fallback=%+v", primary.requests[0], fallback.requests[0])
	}
}

func TestRunSensitiveFallbackRetriesContentFilterResponse(t *testing.T) {
	primary := &scriptedModel{name: "primary", responses: []*loom.ChatResponse{{
		FinishReason: loom.FinishReasonContentFilter,
		Usage:        loom.Usage{PromptTokens: 7, CompletionTokens: 1, TotalTokens: 8},
	}}}
	fallback := &scriptedModel{name: "fallback", responses: []*loom.ChatResponse{{
		Content:      "safe answer",
		FinishReason: loom.FinishReasonStop,
		Usage:        loom.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}}

	turn, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		result, runErr := Run(ctx, w, Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "test task"}},
			Model: primary,
			Tools: loom.NewToolRegistry(),
			SensitiveFallback: &SensitiveFallbackConfig{
				Model: fallback, PrimaryModelID: "primary-route", FallbackModelID: "fallback-route",
			},
		})
		if runErr != nil {
			return runErr
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "content-filter"})
	if err != nil {
		t.Fatal(err)
	}
	if len(primary.requests) != 1 || len(fallback.requests) != 1 {
		t.Fatalf("primary calls=%d fallback calls=%d", len(primary.requests), len(fallback.requests))
	}
	if turn.Usage.PromptTokens != 16 || turn.Usage.CompletionTokens != 3 || turn.Usage.TotalTokens != 19 {
		t.Fatalf("paid fallback usage lost: %+v", turn.Usage)
	}
}

func TestRunSensitiveFallbackSkipsSameConfiguredModelID(t *testing.T) {
	primary := &scriptedModel{name: "primary", responses: []*loom.ChatResponse{{FinishReason: loom.FinishReasonContentFilter}}}
	fallback := &scriptedModel{name: "fallback", responses: []*loom.ChatResponse{{Content: "must not run", FinishReason: loom.FinishReasonStop}}}

	_, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		_, runErr := Run(ctx, w, Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "test task"}},
			Model: primary,
			Tools: loom.NewToolRegistry(),
			SensitiveFallback: &SensitiveFallbackConfig{
				Model: fallback, PrimaryModelID: "same-route", FallbackModelID: "same-route",
			},
		})
		return runErr
	}, loom.RunOptions{ConversationID: "same-model"})
	if !errors.Is(err, loom.ErrContentFilter) {
		t.Fatalf("error=%v, want ErrContentFilter", err)
	}
	if len(fallback.requests) != 0 {
		t.Fatalf("fallback calls=%d, want 0", len(fallback.requests))
	}
}

type sliceStream struct {
	chunks []*loom.Chunk
}

func (s *sliceStream) Recv() (*loom.Chunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	chunk := s.chunks[0]
	s.chunks = s.chunks[1:]
	return chunk, nil
}
func (*sliceStream) Close() error { return nil }

func TestRunExecutesToolsAndFinishes(t *testing.T) {
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{"text":"hello"}`}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "done", FinishReason: loom.FinishReasonStop},
	}}
	text := loom.String("text").Required().Desc("Text to echo.")
	tools := loom.NewToolRegistry(loom.NewArgsTool(loom.MustArgsContract("echo", text), "echo", func(_ context.Context, args loom.Args) (string, error) {
		return `{"text":"` + text.Get(args) + `"}`, nil
	}))

	var result *Result
	turn, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var runErr error
		result, runErr = Run(ctx, w, Config{Model: model, Tools: tools, Messages: []loom.Message{{Role: loom.RoleUser, Content: "go"}}})
		if runErr != nil {
			return runErr
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "test", Input: loom.UserMessage{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != loom.TurnStatusCompleted || result.FinalContent != "done" || result.Steps != 2 {
		t.Fatalf("turn=%+v result=%+v", turn, result)
	}
	if len(result.Messages) != 4 || result.Messages[2].Role != loom.RoleTool {
		t.Fatalf("messages = %+v", result.Messages)
	}
}

func TestRunSoftLandingDisablesTools(t *testing.T) {
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{}`}}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "landed", FinishReason: loom.FinishReasonStop},
	}}
	tools := loom.NewToolRegistry(loom.NewArgsTool(loom.MustArgsContract("echo"), "echo", func(context.Context, loom.Args) (string, error) { return "ok", nil }))

	var result *Result
	_, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var runErr error
		result, runErr = Run(ctx, w, Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "test task"}}, Model: model, Tools: tools, MaxSteps: 1, SoftLandingPrompt: "finish now"})
		if runErr != nil {
			return runErr
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "test", Input: loom.UserMessage{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.SoftLanded || result.FinalContent != "landed" {
		t.Fatalf("result = %+v", result)
	}
	if len(model.requests) != 2 || len(model.requests[1].Tools) != 0 || model.requests[1].ToolChoice == nil || model.requests[1].ToolChoice.Mode != loom.ToolChoiceNone {
		t.Fatalf("final request = %+v", model.requests[1])
	}
}

func TestRunSoftLandsBeforeDeadlineReserve(t *testing.T) {
	model := &scriptedModel{responses: []*loom.ChatResponse{{Content: "landed", FinishReason: loom.FinishReasonStop}}}
	tools := loom.NewToolRegistry(loom.NewArgsTool(loom.MustArgsContract("echo"), "echo", func(context.Context, loom.Args) (string, error) { return "ok", nil }))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var result *Result
	_, err := loom.Run(ctx, func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var runErr error
		result, runErr = Run(ctx, w, Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "test task"}},
			Model:                     model,
			Tools:                     tools,
			SoftLandingReserve:        2 * time.Minute,
			SoftLandingPrompt:         "loop limit",
			DeadlineSoftLandingPrompt: "deadline near",
		})
		if runErr != nil {
			return runErr
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "deadline-reserve"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.SoftLanded || len(model.requests) != 1 || len(model.requests[0].Tools) != 0 {
		t.Fatalf("result=%+v request=%+v", result, model.requests)
	}
	messages := model.requests[0].Messages
	if len(messages) != 2 || messages[1].Content != "deadline near" {
		t.Fatalf("deadline prompt=%+v", messages)
	}
}

func TestRunFinishPolicyContinues(t *testing.T) {
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{Content: "too early", FinishReason: loom.FinishReasonStop},
		{Content: "finished", FinishReason: loom.FinishReasonStop},
	}}
	called := false
	policy := FinishPolicyFunc(func(_ context.Context, _ State, _ *loom.ChatResponse) (FinishDecision, error) {
		if called {
			return FinishDecision{}, nil
		}
		called = true
		return FinishDecision{Continue: true, Instruction: "review again"}, nil
	})

	var result *Result
	_, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var runErr error
		result, runErr = Run(ctx, w, Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "test task"}}, Model: model, Tools: loom.NewToolRegistry(), FinishPolicies: []FinishPolicy{policy}})
		if runErr != nil {
			return runErr
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "test", Input: loom.UserMessage{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalContent != "finished" || result.Steps != 2 {
		t.Fatalf("result = %+v", result)
	}
	if len(model.requests) != 2 || len(model.requests[1].Messages) != 3 || model.requests[1].Messages[2].Content != "review again" {
		t.Fatalf("second request = %+v", model.requests[1])
	}
}

func TestRunEnforcesPerToolLimitWithinOneResponse(t *testing.T) {
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{ToolCalls: []loom.ToolCall{
			{ID: "call-1", Name: "echo", Arguments: `{"n":1}`},
			{ID: "call-2", Name: "echo", Arguments: `{"n":2}`},
		}, FinishReason: loom.FinishReasonToolCalls},
		{Content: "done", FinishReason: loom.FinishReasonStop},
	}}
	invocations := 0
	tools := loom.NewToolRegistry(loom.NewArgsTool(loom.MustArgsContract("echo",
		loom.Uint("n").Required().Desc("Number to echo."),
	), "echo", func(context.Context, loom.Args) (string, error) {
		invocations++
		return "ok", nil
	}, loom.WithBillable()))

	_, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		result, runErr := Run(ctx, w, Config{Messages: []loom.Message{{Role: loom.RoleUser, Content: "test task"}}, Model: model, Tools: tools, ToolCallLimits: map[string]uint64{"echo": 1}})
		if runErr != nil {
			return runErr
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "test", Input: loom.UserMessage{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if invocations != 1 {
		t.Fatalf("invocations = %d, want 1", invocations)
	}
	secondRequest := model.requests[1]
	if len(secondRequest.Messages) != 4 || secondRequest.Messages[3].Role != loom.RoleTool || secondRequest.Messages[3].Content == "" {
		t.Fatalf("second request messages = %+v", secondRequest.Messages)
	}
}
