package zhipuai

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loomagent/loom"
)

func testModel(t *testing.T, handler http.HandlerFunc) *Model {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	m, err := New(Config{APIKey: "test-key", ModelName: "glm-5.3", BaseURL: server.URL + "/api/paas/v4/", Retry: &loom.RetryConfig{MaxRetries: -1}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func request() loom.ChatRequest {
	return loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hello"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: loom.ReasoningEffortLow}}
}
func readRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	if r.URL.Path != "/api/paas/v4/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
		t.Errorf("unexpected endpoint/auth: %s", r.URL.Path)
	}
	var body map[string]any
	if err := jsonv2.UnmarshalRead(r.Body, &body); err != nil {
		t.Error(err)
	}
	return body
}
func TestChatAndToolHistory(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		b := readRequest(t, r)
		if b["model"] != "glm-5.3" || b["reasoning_effort"] != "low" || b["thinking"].(map[string]any)["type"] != "enabled" {
			t.Errorf("request: %v", b)
		}
		if _, ok := b["reasoning"]; ok {
			t.Error("OpenRouter reasoning leaked")
		}
		msgs := b["messages"].([]any)
		assistant := msgs[1].(map[string]any)
		if assistant["reasoning_content"] != "plan" || msgs[2].(map[string]any)["tool_call_id"] != "call-1" {
			t.Error("lost reasoning/tool history")
		}
		if _, ok := b["stop"].([]any); !ok {
			t.Error("stop must be an array even for a single item")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"model":"glm-5.3","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"reasoning_content":"next","tool_calls":[{"id":"call-2","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`)
	})
	req := request()
	req.Stop = []string{"END"}
	req.Messages = append(req.Messages, loom.Message{Role: loom.RoleAssistant, ReasoningContent: "plan", ToolCalls: []loom.ToolCall{{ID: "call-1", Name: "lookup", Arguments: "{}"}}}, loom.Message{Role: loom.RoleTool, ToolCallID: "call-1", Content: "found"})
	out, err := m.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReasoningContent != "next" || len(out.ToolCalls) != 1 || out.ToolCalls[0].ID != "call-2" || out.Usage.CachedTokens != 3 || out.Usage.TotalTokens != 18 || out.Usage.ReasoningTokensKnown {
		t.Fatalf("response: %+v", out)
	}
}
func TestStreamProtocol(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		b := readRequest(t, r)
		if b["stream"] != true || b["tool_stream"] != true || b["reasoning_effort"] != "low" {
			t.Errorf("request: %v", b)
		}
		if _, ok := b["stream_options"]; ok {
			t.Error("unsupported stream_options")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"choices":[{"delta":{"reasoning_content":"plan"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"lookup","arguments":"{"}},{"index":1,"id":"b","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18,"completion_tokens_details":{"reasoning_tokens":4}}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})
	req := request()
	req.Tools = []*loom.ToolInfo{{Name: "lookup"}}
	stream, err := m.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var reasoning string
	var usage loom.Usage
	args := map[int]string{}
	var finish loom.FinishReason
	for {
		c, e := stream.Recv()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		reasoning += c.ReasoningContentDelta
		for _, tc := range c.ToolCallDeltas {
			args[tc.Index] += tc.Arguments
		}
		if c.Usage != nil {
			usage = *c.Usage
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}
	if reasoning != "plan" || args[0] != "{}" || args[1] != "{}" || usage.ReasoningTokens != 4 || !usage.ReasoningTokensKnown || finish != loom.FinishReasonToolCalls {
		t.Fatalf("stream: %q %v %+v %s", reasoning, args, usage, finish)
	}
}
func TestStreamFailuresAndNoReplay(t *testing.T) {
	for _, terminal := range []string{"", "network_error", "sensitive", "model_context_window_exceeded", "unknown"} {
		t.Run(terminal, func(t *testing.T) {
			var calls atomic.Int32
			m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
				if terminal != "" {
					_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}]}\n\n", terminal)
				}
			})
			stream, err := m.Stream(context.Background(), request())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			if _, err = stream.Recv(); err != nil {
				t.Fatal(err)
			}
			_, err = stream.Recv()
			if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("expected failure, got %v", err)
			}
			if terminal == "sensitive" && !errors.Is(err, loom.ErrSensitiveContentRisk) {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatalf("replayed partial stream %d times", calls.Load())
			}
		})
	}
}
func TestControlsAndCapabilities(t *testing.T) {
	m, err := New(Config{APIKey: "key", ModelName: "glm-5.3"})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []loom.ReasoningMode{loom.ReasoningModeDisabled, loom.ReasoningModeEnabled} {
		req := request()
		req.Reasoning = loom.Reasoning{Mode: mode}
		out, err := m.buildRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := jsonv2.Marshal(out)
		if !strings.Contains(string(b), `"type":"`+string(mode)+`"`) {
			t.Fatalf("not forwarded: %s", b)
		}
	}
	m.capabilities = loom.ModelCapabilities{Reasoning: loom.ReasoningSupportAlwaysOn, ReasoningEfforts: []loom.ReasoningEffort{loom.ReasoningEffortLow, loom.ReasoningEffortHigh, loom.ReasoningEffortMax}}
	req := request()
	req.Reasoning = loom.Reasoning{Mode: loom.ReasoningModeDisabled}
	if _, err := m.buildRequest(req); err == nil {
		t.Error("always_on disabled should fail locally")
	}
	for _, effort := range []loom.ReasoningEffort{loom.ReasoningEffortLow, loom.ReasoningEffortHigh, loom.ReasoningEffortMax} {
		req = request()
		req.Reasoning.Effort = effort
		if _, err := m.buildRequest(req); err != nil {
			t.Fatal(err)
		}
	}
	req = request()
	req.Reasoning.Effort = loom.ReasoningEffortMedium
	if _, err := m.buildRequest(req); err == nil {
		t.Error("undeclared effort accepted")
	}
	req = request()
	req.StructuredOutput = &loom.StructuredOutput{Mode: loom.StructuredOutputJSONSchema}
	if _, err := m.buildRequest(req); err == nil || errors.Is(err, loom.ErrUnsupportedCapability) {
		t.Fatalf("schema: %v", err)
	}
	for _, mode := range []loom.ToolChoiceMode{loom.ToolChoiceNone, loom.ToolChoiceRequired, loom.ToolChoiceSpecific} {
		req = request()
		req.ToolChoice = &loom.ToolChoice{Mode: mode}
		if _, err := m.buildRequest(req); !errors.Is(err, loom.ErrUnsupportedCapability) {
			t.Fatalf("tool choice: %v", err)
		}
	}
}
func TestBusinessErrorsAndSDKRetryDisabled(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		class  loom.ErrorClass
	}{
		{"1113", 429, loom.ErrorClassPermanent}, {"1302", 429, loom.ErrorClassRateLimit}, {"1305", 429, loom.ErrorClassTransient}, {"1211", 400, loom.ErrorClassPermanent}, {"1301", 400, loom.ErrorClassPermanent}, {"9999", 429, loom.ErrorClassTransient},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var calls atomic.Int32
			m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-ID", "req-1")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"test failure"}}`, tc.code)
			})
			// raw call isolates SDK retries from Loom's separately tested policy.
			req, err := m.buildRequest(request())
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.chatRaw(context.Background(), req)
			e, ok := errors.AsType[*APIError](err)
			if !ok || e.Code != tc.code || e.RequestID != "req-1" {
				t.Fatalf("error: %v", err)
			}
			if calls.Load() != 1 || (classifier{}).ClassifyError(err) != tc.class || (classifier{}).RetryAfter(err) != 2*time.Second {
				t.Fatalf("classification/retries: %v calls=%d", err, calls.Load())
			}
			if e.RejectsCapability("thinking") {
				t.Error("operational error treated as capability rejection")
			}
		})
	}
}
func TestCancellation(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.Chat(ctx, request())
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLocalizedCapabilityRejection(t *testing.T) {
	err := &APIError{StatusCode: 400, Code: "1210", Message: "该模型始终思考，不支持关闭思考；请使用 low、high 或 max。"}
	if !err.RejectsCapability("thinking") {
		t.Fatal("localized thinking rejection not recognized")
	}
	if !err.RejectsCapability("reasoning_effort") {
		t.Fatal("explicit allowed effort list not recognized")
	}
	if err.RejectsCapability("response_format") {
		t.Fatal("unrelated output capability rejected")
	}
	err.Message = "该模型始终思考，不支持关闭思考"
	if err.RejectsCapability("reasoning_effort") {
		t.Fatal("thinking-only message must not reject an effort")
	}
}
