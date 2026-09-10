package deepseek

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loomagent/loom"
)

func TestUsagePresenceOnWire(t *testing.T) {
	cases := []struct {
		name, usage string
		known       bool
		tokens      uint64
	}{
		{"missing usage", "", false, 0},
		{"null usage", `,"usage":null`, false, 0},
		{"missing details", `,"usage":{"total_tokens":12}`, false, 0},
		{"null details", `,"usage":{"completion_tokens_details":null}`, false, 0},
		{"missing count", `,"usage":{"completion_tokens_details":{}}`, false, 0},
		{"null count", `,"usage":{"completion_tokens_details":{"reasoning_tokens":null}}`, false, 0},
		{"zero", `,"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,"prompt_cache_hit_tokens":2,"completion_tokens_details":{"reasoning_tokens":0}}`, true, 0},
		{"positive", `,"usage":{"completion_tokens_details":{"reasoning_tokens":595}}`, true, 595},
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				t.Parallel()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test" {
						t.Errorf("request contract: %s", r.URL.Path)
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					if body["thinking"].(map[string]any)["type"] != "disabled" {
						t.Error("reasoning switch changed")
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"model\":\"deepseek-future\",\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
						fmt.Fprintf(w, "data: {\"choices\":[]%s}\n\n", tc.usage)
						// Explicit missing telemetry after a known frame must not inherit it.
						fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{}}\n\ndata: [DONE]\n\n")
					} else {
						fmt.Fprintf(w, `{"model":"deepseek-future","choices":[{"message":{"content":"answer"},"finish_reason":"stop"}]%s}`, tc.usage)
					}
				}))
				defer server.Close()
				m, err := New(Config{APIKey: "test", BaseURL: server.URL, ModelName: "deepseek-future", Retry: &loom.RetryConfig{Mode: loom.RetryModeDisabled}})
				if err != nil {
					t.Fatal(err)
				}
				req := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hello"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}}
				var got loom.Usage
				if stream {
					s, err := m.Stream(t.Context(), req)
					if err != nil {
						t.Fatal(err)
					}
					defer s.Close()
					first, err := s.Recv()
					if err != nil || first.ContentDelta != "answer" || first.Usage != nil {
						t.Fatalf("first=%+v err=%v", first, err)
					}
					chunk, err := s.Recv()
					if err != nil {
						t.Fatal(err)
					}
					if chunk.Usage != nil {
						got = *chunk.Usage
					}
					last, err := s.Recv()
					if err != nil || last.Usage == nil || last.Usage.ReasoningTokensKnown {
						t.Fatalf("leaked presence: %+v %v", last, err)
					}
					if _, err := s.Recv(); !errors.Is(err, io.EOF) {
						t.Fatalf("end=%v", err)
					}
				} else {
					out, err := m.Chat(t.Context(), req)
					if err != nil {
						t.Fatal(err)
					}
					if out.Content != "answer" {
						t.Fatal(out.Content)
					}
					got = out.Usage
				}
				if got.ReasoningTokensKnown != tc.known || got.ReasoningTokens != tc.tokens {
					t.Fatalf("got=%+v want known=%v tokens=%d", got, tc.known, tc.tokens)
				}
				if tc.name == "zero" && (got.PromptTokens != 5 || got.CompletionTokens != 7 || got.TotalTokens != 12 || got.CachedTokens != 2) {
					t.Fatalf("usage fields lost: %+v", got)
				}
			})
		}
	}
}
