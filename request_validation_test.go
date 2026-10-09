package loom

import (
	"errors"
	"testing"
	"testing/synctest"
)

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
