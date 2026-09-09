package loom_test

import (
	"context"
	json "encoding/json/v2"
	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/ark"
	"github.com/loomagent/loom/providers/deepseek"
	"github.com/loomagent/loom/providers/openrouter"
	"github.com/loomagent/loom/providers/zhipuai"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeclaredNativeEffortsReachProviderUnchanged(t *testing.T) {
	for _, provider := range []string{"ark", "deepseek", "openrouter", "zhipuai"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]any
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Error(err)
				}
				got := body["reasoning_effort"]
				if provider == "openrouter" {
					got = body["reasoning"].(map[string]any)["effort"]
				}
				if got != "Exact-Level" {
					t.Errorf("effort transformed: %v", body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"response","model":"manual-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
			}))
			defer server.Close()
			caps := &loom.ModelCapabilities{Reasoning: loom.ReasoningSupportToggleable, ReasoningEfforts: []loom.ReasoningEffort{"Exact-Level"}}
			var model loom.ChatModel
			var err error
			switch provider {
			case "ark":
				model, err = ark.New(ark.Config{APIKey: "test", ModelName: "manual-model", BaseURL: server.URL, Capabilities: caps})
			case "deepseek":
				model, err = deepseek.New(deepseek.Config{APIKey: "test", ModelName: "manual-model", BaseURL: server.URL, Capabilities: caps})
			case "openrouter":
				model, err = openrouter.New(openrouter.Config{APIKey: "test", ModelName: "manual-model", BaseURL: server.URL, Capabilities: caps})
			case "zhipuai":
				model, err = zhipuai.New(zhipuai.Config{APIKey: "test", ModelName: "manual-model", BaseURL: server.URL, Capabilities: caps})
			}
			if err != nil {
				t.Fatal(err)
			}
			req := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: "Exact-Level"}}
			if _, err = model.Chat(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			req.Reasoning.Effort = "exact-level"
			if _, err = model.Chat(context.Background(), req); err == nil {
				t.Fatal("undeclared case variant accepted")
			}
			if calls != 1 {
				t.Fatalf("invalid request reached network: %d", calls)
			}
		})
	}
}
