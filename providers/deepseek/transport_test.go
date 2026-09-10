package deepseek

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/loomagent/loom"
	goseek "github.com/storynap/goseek"
)

func TestSchemaRequestReachesUnknownModel(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test" {
					t.Errorf("unexpected endpoint/auth: %s", r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body["model"] != "deepseek-future" || body["reasoning_effort"] != "future-effort" {
					t.Errorf("model/effort changed: %v", body)
				}
				format := body["response_format"].(map[string]any)
				schema := format["json_schema"].(map[string]any)
				if format["type"] != "json_schema" || schema["strict"] != true || schema["name"] != "future_contract" || schema["schema"].(map[string]any)["type"] != "object" {
					t.Errorf("schema lost on wire: %v", format)
				}
				if _, exists := body["logprobs"]; exists {
					t.Error("invented logprobs parameter")
				}
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"model\":\"deepseek-future\",\"choices\":[{\"delta\":{\"content\":\"{\\\"ok\\\":true}\"},\"finish_reason\":\"stop\"}]}\n\n")
					fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"total_tokens\":12,\"completion_tokens_details\":{\"reasoning_tokens\":0}}}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"model":"deepseek-future","choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"total_tokens":12,"completion_tokens_details":{"reasoning_tokens":0}}}`)
				}
			}))
			defer server.Close()
			caps := loom.ReasoningProbeCapabilities(false)
			m, err := New(Config{APIKey: "test", BaseURL: server.URL + "/v1", ModelName: "deepseek-future", Capabilities: &caps, Retry: &loom.RetryConfig{Mode: loom.RetryModeDisabled}})
			if err != nil {
				t.Fatal(err)
			}
			req := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "Use the response schema."}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: "future-effort"}, StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputJSONSchema, Name: "future_contract", Schema: &jsonschema.Schema{Type: "object"}}}
			if streaming {
				stream, err := m.Stream(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				var content string
				var usage *loom.Usage
				for {
					chunk, err := stream.Recv()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					content += chunk.ContentDelta
					if chunk.Usage != nil {
						usage = chunk.Usage
					}
				}
				if content != `{"ok":true}` || usage == nil || !usage.ReasoningTokensKnown || usage.ReasoningTokens != 0 || usage.TotalTokens != 12 {
					t.Fatalf("content=%s usage=%+v", content, usage)
				}
			} else {
				out, err := m.Chat(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				if out.Content != `{"ok":true}` || !out.Usage.ReasoningTokensKnown || out.Usage.ReasoningTokens != 0 || out.Usage.TotalTokens != 12 {
					t.Fatalf("response=%+v", out)
				}
			}
			if calls != 1 {
				t.Fatalf("upstream calls=%d", calls)
			}
		})
	}
}

func TestReasoningTelemetryPresence(t *testing.T) {
	for _, tt := range []struct {
		raw    string
		known  bool
		tokens uint64
	}{
		{`{}`, false, 0}, {`{"completion_tokens_details":{}}`, false, 0},
		{`{"completion_tokens_details":{"reasoning_tokens":null}}`, false, 0},
		{`{"completion_tokens_details":{"reasoning_tokens":0}}`, true, 0},
		{`{"completion_tokens_details":{"reasoning_tokens":595}}`, true, 595},
	} {
		var wire chatUsage
		if err := json.Unmarshal([]byte(tt.raw), &wire); err != nil {
			t.Fatal(err)
		}
		got := translateUsage(&wire)
		if got.ReasoningTokensKnown != tt.known || got.ReasoningTokens != tt.tokens {
			t.Fatalf("%s: %+v", tt.raw, got)
		}
	}
}

func TestSchemaUpstreamErrorAndDeclaredBusinessGuard(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"This response_format type is unavailable now"}}`)
	}))
	defer server.Close()
	req := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "JSON"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}, StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputJSONSchema, Name: "probe", Schema: &jsonschema.Schema{Type: "object"}}}
	m, err := New(Config{APIKey: "test", BaseURL: server.URL, ModelName: "deepseek-flash", Retry: &loom.RetryConfig{Mode: loom.RetryModeDisabled}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Chat(t.Context(), req)
	var apiErr *goseek.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 || apiErr.Header.Get("Retry-After") != "7" || errors.Is(err, loom.ErrUnsupportedCapability) {
		t.Fatalf("upstream error lost: %v", err)
	}
	m.capabilities = loom.ModelCapabilities{StructuredOutput: loom.StructuredOutputJSONObject}
	_, err = m.Chat(t.Context(), req)
	var local *loom.RequestValidationError
	if !errors.As(err, &local) || calls != 1 {
		t.Fatalf("business declaration bypassed: calls=%d err=%v", calls, err)
	}
}
