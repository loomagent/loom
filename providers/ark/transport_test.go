package ark

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loomagent/loom"
)

// testModel points a model at an in-memory test server. The API key is a placeholder: the
// fake server never checks it, and nothing here reaches the network.
func testModel(t *testing.T, handler http.HandlerFunc) *Model {
	t.Helper()
	server := httptest.NewTestServer(t, handler)
	m, err := New(Config{
		APIKey:     "test",
		ModelName:  "ep-test",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Retry:      &loom.RetryConfig{Mode: loom.RetryModeDisabled},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const completionBody = `{
	"id": "x", "model": "ep-2026",
	"choices": [{
		"index": 0,
		"message": {
			"role": "assistant", "content": "answer", "reasoning_content": "why",
			"tool_calls": [{"id": "c1", "type": "function", "function": {"name": "lookup", "arguments": "{\"q\":\"x\"}"}}]
		},
		"finish_reason": "tool_calls"
	}],
	"usage": {
		"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7,
		"prompt_tokens_details": {"cached_tokens": 1},
		"completion_tokens_details": {"reasoning_tokens": 2}
	}
}`

// One completion, end to end: the response the endpoint returns has to arrive as a
// loom.ChatResponse with every field mapped, including the reasoning evidence that is
// captured before the SDK decodes the body.
func TestChatMapsResponse(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, completionBody)
	})

	resp, err := m.Chat(t.Context(), loom.ChatRequest{
		Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
		Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "answer" || resp.ReasoningContent != "why" || resp.Model != "ep-2026" {
		t.Fatalf("response = %+v", resp)
	}
	if resp.FinishReason != loom.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "c1" ||
		resp.ToolCalls[0].Name != "lookup" || resp.ToolCalls[0].Arguments != `{"q":"x"}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	usage := resp.Usage
	if usage.PromptTokens != 3 || usage.CompletionTokens != 4 || usage.TotalTokens != 7 || usage.CachedTokens != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.ReasoningTokens != 2 || !usage.ReasoningTokensKnown {
		t.Fatalf("reasoning evidence = %+v", usage)
	}
}

// What the endpoint actually receives: the tools with their parameter schema, the forced
// tool choice, strict structured output, and the reasoning switch.
func TestChatSendsDeclaredToolsAndStructuredOutput(t *testing.T) {
	var sent map[string]any
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if err := jsonv2.Unmarshal(raw, &sent); err != nil {
			t.Errorf("decode request %s: %v", raw, err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"ep-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})

	_, err := m.Chat(t.Context(), loom.ChatRequest{
		Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
		Tools: []*loom.ToolInfo{{
			Name:        "search",
			Description: "Search the web.",
			Parameters:  &loom.Schema{Type: "object", Properties: map[string]*loom.Schema{"q": {Type: "string"}}},
		}},
		ToolChoice: &loom.ToolChoice{Mode: loom.ToolChoiceSpecific, Name: "search"},
		StructuredOutput: &loom.StructuredOutput{
			Mode:   loom.StructuredOutputJSONSchema,
			Name:   "result",
			Schema: &loom.Schema{Type: "object"},
		},
		Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled},
	})
	if err != nil {
		t.Fatal(err)
	}

	tools, ok := sent["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", sent["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	function, _ := tool["function"].(map[string]any)
	if tool["type"] != "function" || function["name"] != "search" || function["description"] != "Search the web." {
		t.Fatalf("tool = %#v", tools[0])
	}
	parameters, _ := function["parameters"].(map[string]any)
	if parameters["type"] != "object" {
		t.Fatalf("parameter schema = %#v", function["parameters"])
	}

	choice, _ := sent["tool_choice"].(map[string]any)
	chosen, _ := choice["function"].(map[string]any)
	if choice["type"] != "function" || chosen["name"] != "search" {
		t.Fatalf("tool_choice = %#v", sent["tool_choice"])
	}

	format, _ := sent["response_format"].(map[string]any)
	schema, _ := format["json_schema"].(map[string]any)
	if format["type"] != "json_schema" || schema["name"] != "result" || schema["strict"] != true {
		t.Fatalf("response_format = %#v", sent["response_format"])
	}

	thinking, _ := sent["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v", sent["thinking"])
	}
}

// The streamed path has its own mapping, and the deltas have to reassemble into the same
// answer the unary path would have produced.
func TestStreamMapsDeltas(t *testing.T) {
	var sent map[string]any
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if err := jsonv2.Unmarshal(raw, &sent); err != nil {
			t.Errorf("decode request %s: %v", raw, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"model\":\"ep-2026\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"he\",\"reasoning_content\":\"th\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"model\":\"ep-2026\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: {\"model\":\"ep-2026\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	stream, err := m.Stream(t.Context(), loom.ChatRequest{
		Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
		Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	var content, reasoning string
	var calls []loom.ToolCallDelta
	var finish loom.FinishReason
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
		reasoning += chunk.ReasoningContentDelta
		calls = append(calls, chunk.ToolCallDeltas...)
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if content != "hello" || reasoning != "th" {
		t.Fatalf("content/reasoning = %q/%q", content, reasoning)
	}
	if finish != loom.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q", finish)
	}
	if len(calls) != 1 || calls[0].Index != 0 || calls[0].ID != "c1" ||
		calls[0].Name != "lookup" || calls[0].Arguments != "{}" {
		t.Fatalf("tool call deltas = %+v", calls)
	}
	if usage == nil || usage.PromptTokens != 1 || usage.CompletionTokens != 2 || usage.TotalTokens != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	// The stream path has to ask for usage, or the last frame would never carry it.
	if sent["stream"] != true {
		t.Fatalf("stream = %#v", sent["stream"])
	}
	options, _ := sent["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("stream_options = %#v", sent["stream_options"])
	}
}
