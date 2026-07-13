package react

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/loomagent/loom"
)

type scriptedModel struct {
	responses []*loom.ChatResponse
	requests  []loom.ChatRequest
}

func (m *scriptedModel) Name() string { return "test/model" }
func (m *scriptedModel) Capabilities() loom.ModelCapabilities {
	return loom.ModelCapabilities{}
}
func (m *scriptedModel) Chat(context.Context, loom.ChatRequest) (*loom.ChatResponse, error) {
	return nil, errors.New("unexpected Chat call")
}
func (m *scriptedModel) Stream(_ context.Context, req loom.ChatRequest) (loom.Stream, error) {
	m.requests = append(m.requests, req)
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
	tools := loom.NewToolRegistry(loom.NewTool("echo", "echo", nil, func(_ context.Context, args string) (string, error) { return args, nil }))

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
	tools := loom.NewToolRegistry(loom.NewTool("echo", "echo", nil, func(context.Context, string) (string, error) { return "ok", nil }))

	var result *Result
	_, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var runErr error
		result, runErr = Run(ctx, w, Config{Model: model, Tools: tools, MaxSteps: 1, SoftLandingPrompt: "finish now"})
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
		result, runErr = Run(ctx, w, Config{Model: model, Tools: loom.NewToolRegistry(), FinishPolicies: []FinishPolicy{policy}})
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
	if len(model.requests) != 2 || len(model.requests[1].Messages) != 2 || model.requests[1].Messages[1].Content != "review again" {
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
	tools := loom.NewToolRegistry(loom.NewTool("echo", "echo", nil, func(context.Context, string) (string, error) {
		invocations++
		return "ok", nil
	}))

	_, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		result, runErr := Run(ctx, w, Config{Model: model, Tools: tools, ToolCallLimits: map[string]uint64{"echo": 1}})
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
	if len(secondRequest.Messages) != 3 || secondRequest.Messages[2].Role != loom.RoleTool || secondRequest.Messages[2].Content == "" {
		t.Fatalf("second request messages = %+v", secondRequest.Messages)
	}
}
