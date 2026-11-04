package loom_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/ark"
	"github.com/loomagent/loom/providers/deepseek"
	"github.com/loomagent/loom/providers/openrouter"
	"github.com/loomagent/loom/providers/zhipuai"
)

func TestProviderCapabilitiesStayAtConstruction(t *testing.T) {
	constructors := []struct {
		name     string
		newModel func(string, *http.Client, *loom.ModelCapabilities) (loom.ChatModel, error)
	}{
		{"ark", func(url string, client *http.Client, caps *loom.ModelCapabilities) (loom.ChatModel, error) {
			return ark.New(ark.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: url, HTTPClient: client, Capabilities: caps})
		}},
		{"deepseek", func(url string, client *http.Client, caps *loom.ModelCapabilities) (loom.ChatModel, error) {
			return deepseek.New(deepseek.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: url, HTTPClient: client, Capabilities: caps})
		}},
		{"openrouter", func(url string, client *http.Client, caps *loom.ModelCapabilities) (loom.ChatModel, error) {
			return openrouter.New(openrouter.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: url, HTTPClient: client, Capabilities: caps})
		}},
		{"zhipuai", func(url string, client *http.Client, caps *loom.ModelCapabilities) (loom.ChatModel, error) {
			return zhipuai.New(zhipuai.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: url, HTTPClient: client, Capabilities: caps})
		}},
	}
	for _, provider := range constructors {
		for _, mutation := range []string{"constructor input", "returned capabilities"} {
			for _, entry := range []string{"chat", "stream"} {
				t.Run(provider.name+"/"+mutation+"/"+entry, func(t *testing.T) {
					var requests atomic.Int64
					server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						requests.Add(1)
						// Both entry points receive a normal response from the fixture.
						if entry == "stream" {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"model":"fake-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
					}))
					caps := loom.ModelCapabilities{Reasoning: loom.ReasoningSupportToggleable, ReasoningEfforts: []loom.ReasoningEffort{"high", "low"}}
					model, err := provider.newModel(server.URL, server.Client(), &caps)
					if err != nil {
						t.Fatal(err)
					}
					if mutation == "constructor input" {
						caps.ReasoningEfforts[0] = "max"
					} else {
						returned := model.Capabilities()
						returned.ReasoningEfforts[0] = "max"
					}
					// A caller's edit must not change which requests reach the provider.
					request := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hello"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: "high"}}
					if err := callCapabilitySnapshotModel(t.Context(), model, entry, request); err != nil {
						t.Fatalf("original declared effort stopped working: %v", err)
					}
					request.Reasoning.Effort = "max"
					if err := callCapabilitySnapshotModel(t.Context(), model, entry, request); err == nil {
						t.Fatal("caller edit admitted an undeclared effort")
					} else if _, ok := errors.AsType[*loom.RequestValidationError](err); !ok {
						t.Fatalf("undeclared effort: expected local validation, got %v", err)
					}
					if requests.Load() != 1 {
						t.Fatalf("provider received %d requests, want only the declared effort", requests.Load())
					}
					// Independent readers may edit their own returned snapshots concurrently.
					var readers sync.WaitGroup
					for range 4 {
						readers.Go(func() {
							for range 16 {
								returned := model.Capabilities()
								if !slices.Equal(returned.ReasoningEfforts, []loom.ReasoningEffort{"high", "low"}) {
									t.Error("reader observed another caller's edit")
									return
								}
								returned.ReasoningEfforts[0] = "max"
							}
						})
					}
					readers.Wait()
				})
			}
		}
	}
}

func callCapabilitySnapshotModel(ctx context.Context, model loom.ChatModel, entry string, request loom.ChatRequest) error {
	if entry == "chat" {
		response, err := model.Chat(ctx, request)
		if err == nil && (response == nil || response.Content != "ok") {
			return fmt.Errorf("unexpected response: %+v", response)
		}
		return err
	}
	stream, err := model.Stream(ctx, request)
	if err != nil {
		return err
	}
	defer stream.Close()
	var content string
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if content != "ok" {
				return fmt.Errorf("unexpected stream content: %q", content)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if chunk != nil {
			content += chunk.ContentDelta
		}
	}
}
