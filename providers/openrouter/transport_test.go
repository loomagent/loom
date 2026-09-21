package openrouter

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/loomagent/loom"
)

func testModel(t *testing.T, handler http.HandlerFunc) *Model {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	m, err := New(Config{
		APIKey:    "test",
		BaseURL:   server.URL + "/api/v1",
		ModelName: "x-ai/grok-4.3",
		Retry:     &loom.RetryConfig{Mode: loom.RetryModeDisabled},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNameAndCapabilities(t *testing.T) {
	caps := loom.ModelCapabilities{StructuredOutput: loom.StructuredOutputJSONSchema}
	m, err := New(Config{APIKey: "test", ModelName: "x-ai/grok-4.3", Capabilities: &caps})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Name(); got != "openrouter/x-ai/grok-4.3" {
		t.Fatalf("Name() = %q", got)
	}
	if m.Capabilities().StructuredOutput != loom.StructuredOutputJSONSchema {
		t.Fatalf("Capabilities() = %+v", m.Capabilities())
	}
}

// Chat maps a provider response into Loom's shape: content, the OpenRouter
// "reasoning" extra field, tool calls, the finish reason, and usage.
func TestChatMapsResponse(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("request contract: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"model": "x-ai/grok-4.3",
			"choices": [{
				"message": {
					"content": "answer",
					"reasoning": "because",
					"tool_calls": [{"id": "c1", "type": "function", "function": {"name": "lookup", "arguments": "{\"q\":1}"}}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3, "completion_tokens_details": {"reasoning_tokens": 4}}
		}`)
	})

	out, err := m.Chat(t.Context(), loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "answer" || out.ReasoningContent != "because" {
		t.Fatalf("content/reasoning = %q/%q", out.Content, out.ReasoningContent)
	}
	if out.FinishReason != loom.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q", out.FinishReason)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].ID != "c1" || out.ToolCalls[0].Name != "lookup" || out.ToolCalls[0].Arguments != `{"q":1}` {
		t.Fatalf("tool calls = %+v", out.ToolCalls)
	}
	if out.Usage.TotalTokens != 3 || out.Usage.ReasoningTokens != 4 || !out.Usage.ReasoningTokensKnown {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestChatRejectsEmptyChoices(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"m","choices":[]}`)
	})
	if _, err := m.Chat(t.Context(), loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}}); err == nil {
		t.Fatal("a response with no choices accepted")
	}
}

func TestStreamMapsDeltas(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"he\",\"reasoning\":\"th\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: {\"model\":\"m\",\"choices\":[],\"usage\":{\"total_tokens\":9}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	stream, err := m.Stream(t.Context(), loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	var content, reasoning string
	var calls []loom.ToolCallDelta
	var usage *loom.Usage
	var finish loom.FinishReason
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content += chunk.ContentDelta
		reasoning += chunk.ReasoningContentDelta
		calls = append(calls, chunk.ToolCallDeltas...)
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
	}
	if content != "hello" || reasoning != "th" {
		t.Fatalf("content/reasoning = %q/%q", content, reasoning)
	}
	if finish != loom.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q", finish)
	}
	if len(calls) != 1 || calls[0].ID != "c1" || calls[0].Name != "lookup" || calls[0].Arguments != "{}" {
		t.Fatalf("tool call deltas = %+v", calls)
	}
	if usage == nil || usage.TotalTokens != 9 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestStreamSurfacesUpstreamError(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"temporarily unavailable"}}`)
	})
	stream, err := m.Stream(t.Context(), loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}})
	if err == nil {
		if _, recvErr := stream.Recv(); recvErr == nil {
			t.Fatal("upstream error accepted")
		}
	}
}

func TestTranslateFinishReason(t *testing.T) {
	for raw, want := range map[string]loom.FinishReason{
		"stop":           loom.FinishReasonStop,
		"length":         loom.FinishReasonLength,
		"tool_calls":     loom.FinishReasonToolCalls,
		"function_call":  loom.FinishReasonToolCalls,
		"content_filter": loom.FinishReasonContentFilter,
		"":               "",
		"something-else": loom.FinishReasonStop,
	} {
		if got := translateFinishReason(raw); got != want {
			t.Errorf("translateFinishReason(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestTranslateUsagePresence(t *testing.T) {
	if got := translateUsage(nil); got != (loom.Usage{}) {
		t.Fatalf("nil usage = %+v", got)
	}
	var usage openai.CompletionUsage
	if err := json.Unmarshal([]byte(`{
		"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3,
		"prompt_tokens_details": {"cached_tokens": 4},
		"completion_tokens_details": {"reasoning_tokens": 5}
	}`), &usage); err != nil {
		t.Fatal(err)
	}
	got := translateUsage(&usage)
	if got.PromptTokens != 1 || got.CompletionTokens != 2 || got.TotalTokens != 3 || got.CachedTokens != 4 {
		t.Fatalf("usage = %+v", got)
	}
	if got.ReasoningTokens != 5 || !got.ReasoningTokensKnown {
		t.Fatalf("reasoning presence = %+v", got)
	}
}

func TestTranslateToolChoice(t *testing.T) {
	if got := translateToolChoice(nil); got.OfAuto.Valid() {
		t.Fatalf("nil tool choice = %+v", got)
	}
	for mode, want := range map[loom.ToolChoiceMode]string{
		loom.ToolChoiceAuto:     "auto",
		loom.ToolChoiceNone:     "none",
		loom.ToolChoiceRequired: "required",
	} {
		choice := translateToolChoice(&loom.ToolChoice{Mode: mode})
		if choice.OfAuto.Value != want {
			t.Errorf("mode %q mapped to %q, want %q", mode, choice.OfAuto.Value, want)
		}
	}
	if got := translateToolChoice(&loom.ToolChoice{Mode: loom.ToolChoiceSpecific, Name: "lookup"}); got.OfFunctionToolChoice == nil || got.OfFunctionToolChoice.Function.Name != "lookup" {
		t.Errorf("specific choice = %+v", got)
	}
	// An unknown mode is not sent at all, leaving the provider default.
	if got := translateToolChoice(&loom.ToolChoice{Mode: loom.ToolChoiceMode("unknown")}); got.OfAuto.Valid() || got.OfFunctionToolChoice != nil {
		t.Errorf("unknown mode sent %+v", got)
	}
}

// The structured reasoning_content field wins over the generic reasoning one.
func TestChatPrefersReasoningContent(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"m","choices":[{"message":{"content":"a","reasoning":"generic","reasoning_content":"structured"},"finish_reason":"stop"}]}`)
	})
	out, err := m.Chat(t.Context(), loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}})
	if err != nil {
		t.Fatal(err)
	}
	if out.ReasoningContent != "structured" {
		t.Fatalf("reasoning = %q, want the structured field", out.ReasoningContent)
	}
}

// A reasoning field that is not a string is ignored rather than fatal.
func TestChatIgnoresNonStringReasoning(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"m","choices":[{"message":{"content":"a","reasoning":{"nested":1}},"finish_reason":"stop"}]}`)
	})
	out, err := m.Chat(t.Context(), loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}})
	if err != nil {
		t.Fatal(err)
	}
	if out.ReasoningContent != "" {
		t.Fatalf("non-string reasoning produced %q", out.ReasoningContent)
	}
}
