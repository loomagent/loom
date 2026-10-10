package loom

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

func Test_user_final_stream_requires_user_history_with_custom_model(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []Message
		valid    bool
	}{
		{name: "empty"},
		{name: "system only", messages: []Message{{Role: RoleSystem, Content: "rules"}}},
		{name: "assistant only", messages: []Message{{Role: RoleAssistant, Content: "answer"}}},
		{name: "tool only", messages: []Message{{Role: RoleTool, ToolCallID: "call-1", Content: "result"}}},
		{name: "user task", messages: []Message{{Role: RoleUser, Content: "task"}}, valid: true},
		{name: "assistant continuation", messages: []Message{{Role: RoleUser, Content: "task"}, {Role: RoleAssistant, Content: "draft"}}, valid: true},
		{name: "tool continuation", messages: []Message{{Role: RoleUser, Content: "task"}, {Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Arguments: "{}"}}}, {Role: RoleTool, ToolCallID: "call-1", Content: "result"}}, valid: true},
	} {
		t.Run("user "+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given a custom model that accepts any input and returns a billable answer.
				model := &finalStreamModel{frames: []*Chunk{{ContentDelta: "ok", FinishReason: FinishReasonStop, Usage: &Usage{TotalTokens: 2}}}}
				sink := NewMemorySink()
				var response *ChatResponse
				// When the host uses the public final-answer entry point through a real Turn.
				turn, err := Run(t.Context(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
					var streamErr error
					response, streamErr = StreamLLMToFinalAnswer(ctx, w, "final", model, ChatRequest{Messages: tc.messages, Reasoning: Reasoning{Mode: ReasoningModeDisabled}})
					return streamErr
				}, RunOptions{ConversationID: tc.name, Sinks: []Sink{sink}, StrictSink: true})
				// Then valid user history completes; missing-user input fails locally without output or usage.
				if tc.valid {
					if err != nil || response == nil || response.Content != "ok" || turn.Status != TurnStatusCompleted {
						t.Fatalf("valid history: response=%+v turn=%+v error=%v", response, turn, err)
					}
					return
				}
				var local *RequestValidationError
				if !errors.Is(err, ErrMissingUserMessage) || !errors.As(err, &local) || response != nil || turn.Status != TurnStatusFailed {
					t.Fatalf("missing user must fail locally: response=%+v turn=%+v error=%v", response, turn, err)
				}
				if len(sink.DeltaEvents()) != 0 || turn.Usage.TotalTokens != 0 {
					t.Fatalf("rejected input produced output or usage: deltas=%v usage=%+v", sink.DeltaEvents(), turn.Usage)
				}
			})
		})
	}
}

func TestUserReceivesLocalValidationWithCustomModel(t *testing.T) {
	for _, entry := range []string{"chat", "rebuilt request", "stream"} {
		t.Run("user "+entry, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given a custom model that would otherwise accept an empty context,
				model := &fakeCallModel{responses: []*ChatResponse{{Content: "accepted"}}}
				req := ChatRequest{Messages: []Message{{Role: RoleSystem, Content: "rules"}}}
				var response *ChatResponse
				var err error
				// When a caller submits or rebuilds a request with no user message,
				switch entry {
				case "chat":
					response, err = CallModel(t.Context(), "test", model, req)
				case "rebuilt request":
					response, err = CallModel(t.Context(), "test", model, ChatRequest{Messages: []Message{{Role: RoleUser, Content: "task"}}}, WithCallModelRequestForModel(func(ChatModel) (ChatRequest, error) { return req, nil }))
				case "stream":
					response, err = StreamLLMToStep(t.Context(), nil, "test", model, req)
				default:
					t.Fatal("unknown entry point")
				}
				// Then even a model outside the built-in providers receives a local error.
				var local *RequestValidationError
				if response != nil || !errors.Is(err, ErrMissingUserMessage) || !errors.As(err, &local) {
					t.Fatalf("response=%+v error=%v", response, err)
				}
			})
		})
	}
}

func Test_user_structured_retry_cannot_replace_a_missing_user_task(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given a retry callback capable of turning a missing task into a valid request.
		contract, _, _ := reviewContract()
		model := &fakeStructuredModel{responses: []string{`{"overall_done":true,"notes":"ok"}`}}
		var retried bool
		// When a structured call begins with only system instructions.
		_, response, err := ChatStructuredArgs(t.Context(), "test", model, ChatRequest{Messages: []Message{{Role: RoleSystem, Content: "rules"}}}, contract,
			WithStructuredAttempts(2), WithStructuredNextRequest(func(_ context.Context, attempt StructuredAttempt) (*ChatRequest, error) {
				retried = true
				req := attempt.Request
				req.Messages = append(req.Messages, Message{Role: RoleUser, Content: "repair"})
				return &req, nil
			}))
		// Then input validation is terminal, before corrective feedback or model output.
		var local *RequestValidationError
		if !errors.Is(err, ErrMissingUserMessage) || !errors.As(err, &local) || response != nil || retried {
			t.Fatalf("missing task was admitted to retry: response=%+v retried=%t error=%v", response, retried, err)
		}
	})
}
