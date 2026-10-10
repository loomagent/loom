package loom_test

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/ark"
	"github.com/loomagent/loom/providers/deepseek"
	"github.com/loomagent/loom/providers/openrouter"
	"github.com/loomagent/loom/providers/zhipuai"
)

func TestUserCannotSendChatWithoutUserMessage(t *testing.T) {
	constructors := []struct {
		name     string
		newModel func(string, *http.Client) (loom.ChatModel, error)
	}{
		{"ark", func(u string, c *http.Client) (loom.ChatModel, error) {
			return ark.New(ark.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: u, HTTPClient: c})
		}},
		{"deepseek", func(u string, c *http.Client) (loom.ChatModel, error) {
			return deepseek.New(deepseek.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: u, HTTPClient: c})
		}},
		{"openrouter", func(u string, c *http.Client) (loom.ChatModel, error) {
			return openrouter.New(openrouter.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: u, HTTPClient: c})
		}},
		{"zhipuai", func(u string, c *http.Client) (loom.ChatModel, error) {
			return zhipuai.New(zhipuai.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: u, HTTPClient: c})
		}},
	}
	cases := []struct {
		name     string
		messages []loom.Message
		valid    bool
	}{
		{"empty context", nil, false},
		{"system only", []loom.Message{{Role: loom.RoleSystem, Content: "classify"}}, false},
		{"two system messages", []loom.Message{{Role: loom.RoleSystem, Content: "classify"}, {Role: loom.RoleSystem, Content: "JSON schema"}}, false},
		{"assistant only", []loom.Message{{Role: loom.RoleAssistant, Content: "answer"}}, false},
		{"tool only", []loom.Message{{Role: loom.RoleTool, ToolCallID: "call-1", Content: "result"}}, false},
		{"user task", []loom.Message{{Role: loom.RoleSystem, Content: "rules"}, {Role: loom.RoleUser, Content: "task"}}, true},
		{"assistant continuation", []loom.Message{{Role: loom.RoleUser, Content: "task"}, {Role: loom.RoleAssistant, Content: "answer"}}, true},
		{"tool continuation", []loom.Message{{Role: loom.RoleUser, Content: "task"}, {Role: loom.RoleAssistant, ToolCalls: []loom.ToolCall{{ID: "call-1", Name: "lookup", Arguments: "{}"}}}, {Role: loom.RoleTool, ToolCallID: "call-1", Content: "result"}}, true},
	}
	for _, provider := range constructors {
		for _, tc := range cases {
			for _, entry := range []string{"chat", "stream", "call model", "stream to step", "final stream"} {
				t.Run("user "+provider.name+" "+entry+" "+tc.name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						// Given an endpoint ledger, requests with no user must never reach it.
						var reached atomic.Bool
						server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							reached.Store(true)
							var body struct {
								Stream bool `json:"stream"`
							}
							if err := jsonv2.UnmarshalRead(r.Body, &body); err != nil {
								t.Error(err)
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							if body.Stream {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
								return
							}
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"model":"fake-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
						}))
						model, err := provider.newModel(server.URL, server.Client())
						if err != nil {
							t.Fatal(err)
						}
						req := loom.ChatRequest{Messages: tc.messages, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}}
						// When a caller uses a direct provider or public model entry point,
						// then it receives a recognizable local error, or the normal answer for valid history.
						var content string
						switch entry {
						case "chat":
							var resp *loom.ChatResponse
							resp, err = model.Chat(t.Context(), req)
							if resp != nil {
								content = resp.Content
							}
						case "call model":
							var resp *loom.ChatResponse
							resp, err = loom.CallModel(t.Context(), "test", model, req, loom.WithModelFailover(loom.FailoverConfig{GetFailoverModel: func(_ context.Context, _ loom.FailoverAttempt) (loom.ChatModel, error) {
								t.Error("invalid input must not select another model")
								return nil, fmt.Errorf("unexpected failover")
							}}))
							if resp != nil {
								content = resp.Content
							}
						case "stream":
							var stream loom.Stream
							stream, err = model.Stream(t.Context(), req)
							if stream != nil {
								defer func() { _ = stream.Close() }()
								for {
									var chunk *loom.Chunk
									chunk, err = stream.Recv()
									if errors.Is(err, io.EOF) {
										err = nil
										break
									}
									if err != nil {
										break
									}
									content += chunk.ContentDelta
								}
							}
						case "stream to step":
							_, err = loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
								resp, streamErr := loom.StreamLLMToStep(ctx, w, "test", model, req)
								if streamErr != nil {
									return streamErr
								}
								content = resp.Content
								return w.FinalAnswer(ctx, content)
							}, loom.RunOptions{ConversationID: "validation"})
						case "final stream":
							_, err = loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
								resp, streamErr := loom.StreamLLMToFinalAnswer(ctx, w, "test", model, req)
								if resp != nil {
									content = resp.Content
								}
								return streamErr
							}, loom.RunOptions{ConversationID: "validation"})
						default:
							t.Fatal("unknown entry point")
						}
						if tc.valid {
							if err != nil || content != "ok" || !reached.Load() {
								t.Fatalf("valid history: content=%q reached=%t error=%v", content, reached.Load(), err)
							}
							return
						}
						var local *loom.RequestValidationError
						if !errors.Is(err, loom.ErrMissingUserMessage) || !errors.As(err, &local) || reached.Load() {
							t.Fatalf("missing user must fail locally before a billable request: reached=%t error=%v", reached.Load(), err)
						}
					})
				})
			}
		}
	}
}
