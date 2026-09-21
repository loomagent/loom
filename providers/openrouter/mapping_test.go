package openrouter

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/loomagent/loom"
)

// apiError builds an SDK error with the status the classifier inspects.
func apiError(status int) error {
	return &openai.Error{
		StatusCode: status,
		Request:    httptest.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil),
		Response:   &http.Response{StatusCode: status, Header: http.Header{}, Status: http.StatusText(status)},
	}
}

// The classification decides whether a turn retries or gives up.
func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want loom.ErrorClass
	}{
		{name: "nil", err: nil, want: loom.ErrorClassUnknown},
		{name: "cancelled", err: context.Canceled, want: loom.ErrorClassPermanent},
		{name: "deadline", err: context.DeadlineExceeded, want: loom.ErrorClassPermanent},
		{name: "rate limit", err: apiError(http.StatusTooManyRequests), want: loom.ErrorClassRateLimit},
		{name: "bad request", err: apiError(http.StatusBadRequest), want: loom.ErrorClassPermanent},
		{name: "unauthorized", err: apiError(http.StatusUnauthorized), want: loom.ErrorClassPermanent},
		{name: "payment required", err: apiError(http.StatusPaymentRequired), want: loom.ErrorClassPermanent},
		{name: "forbidden", err: apiError(http.StatusForbidden), want: loom.ErrorClassPermanent},
		{name: "missing model", err: apiError(http.StatusNotFound), want: loom.ErrorClassPermanent},
		{name: "other client error", err: apiError(http.StatusTeapot), want: loom.ErrorClassPermanent},
		{name: "server error", err: apiError(http.StatusInternalServerError), want: loom.ErrorClassTransient},
		{name: "unavailable", err: apiError(http.StatusServiceUnavailable), want: loom.ErrorClassTransient},
		{name: "network", err: errors.New("connection reset"), want: loom.ErrorClassTransient},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := (classifier{}).ClassifyError(testCase.err); got != testCase.want {
				t.Fatalf("ClassifyError(%v) = %s, want %s", testCase.err, got, testCase.want)
			}
		})
	}
}

func TestIsServiceUnavailable(t *testing.T) {
	if !(classifier{}).IsServiceUnavailable(apiError(http.StatusServiceUnavailable)) {
		t.Fatal("503 must open the shared circuit")
	}
	if (classifier{}).IsServiceUnavailable(apiError(http.StatusInternalServerError)) ||
		(classifier{}).IsServiceUnavailable(errors.New("x")) {
		t.Fatal("only 503 opens the shared circuit")
	}
}

// OpenRouter takes one request-level reasoning object, so the resolved decision has to
// become exactly that, and an unknown decision is an error rather than a silent default.
func TestTranslateReasoning(t *testing.T) {
	if got, err := translateReasoning(loom.ResolvedReasoning{Send: loom.ReasoningSendOmit}); err != nil || got != nil {
		t.Fatalf("omit = %#v (%v)", got, err)
	}
	disabled, err := translateReasoning(loom.ResolvedReasoning{Send: loom.ReasoningSendDisabled})
	if err != nil || disabled["enabled"] != false {
		t.Fatalf("disabled = %#v (%v)", disabled, err)
	}
	if _, present := disabled["effort"]; present {
		t.Fatalf("a disabled call sent an effort: %#v", disabled)
	}
	enabled, err := translateReasoning(loom.ResolvedReasoning{Send: loom.ReasoningSendEnabled, Effort: "high"})
	if err != nil || enabled["enabled"] != true || enabled["effort"] != "high" {
		t.Fatalf("enabled = %#v (%v)", enabled, err)
	}
	bare, err := translateReasoning(loom.ResolvedReasoning{Send: loom.ReasoningSendEnabled})
	if err != nil || bare["enabled"] != true {
		t.Fatalf("enabled without an effort = %#v (%v)", bare, err)
	}
	if _, present := bare["effort"]; present {
		t.Fatalf("an effort appeared from nowhere: %#v", bare)
	}
	if _, err := translateReasoning(loom.ResolvedReasoning{Send: loom.ReasoningSend("other")}); err == nil {
		t.Fatal("an unknown decision must fail")
	}
}

// What the endpoint receives, branch by branch.
func TestBuildRequestBranches(t *testing.T) {
	model, err := New(Config{APIKey: "k", ModelName: "x-ai/grok-4.3"})
	if err != nil {
		t.Fatal(err)
	}
	build := func(t *testing.T, req loom.ChatRequest) map[string]any {
		t.Helper()
		request, err := model.buildRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := jsonv2.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := jsonv2.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	reasoning := loom.Reasoning{Mode: loom.ReasoningModeDisabled}

	t.Run("roles", func(t *testing.T) {
		body := build(t, loom.ChatRequest{Messages: []loom.Message{
			{Role: loom.RoleSystem, Content: "rules"},
			{Role: loom.RoleAssistant, Content: "calling", ToolCalls: []loom.ToolCall{{ID: "c1", Name: "search", Arguments: `{}`}}},
			{Role: loom.RoleTool, Content: "result", ToolCallID: "c1"},
			{Role: loom.RoleUser, Content: "hi"},
		}, Reasoning: reasoning})
		messages := body["messages"].([]any)
		for index, want := range []string{"system", "assistant", "tool", "user"} {
			if got := messages[index].(map[string]any)["role"]; got != want {
				t.Errorf("message %d role = %v, want %q", index, got, want)
			}
		}
		assistant := messages[1].(map[string]any)
		if len(assistant["tool_calls"].([]any)) != 1 {
			t.Errorf("assistant = %#v", assistant)
		}
		if tool := messages[2].(map[string]any); tool["tool_call_id"] != "c1" {
			t.Errorf("tool = %#v", tool)
		}
	})

	t.Run("unknown role is refused", func(t *testing.T) {
		_, err := model.buildRequest(loom.ChatRequest{
			Messages:  []loom.Message{{Role: loom.Role("assistent"), Content: "typo"}},
			Reasoning: reasoning,
		})
		if err == nil || !strings.Contains(err.Error(), `unknown role "assistent"`) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("stop sequences", func(t *testing.T) {
		one := build(t, loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}}, Stop: []string{"END"}, Reasoning: reasoning})
		if one["stop"] != "END" {
			t.Errorf("one stop sequence sent %#v", one["stop"])
		}
		many := build(t, loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}}, Stop: []string{"A", "B"}, Reasoning: reasoning})
		if stops, ok := many["stop"].([]any); !ok || len(stops) != 2 {
			t.Errorf("two stop sequences sent %#v", many["stop"])
		}
	})

	t.Run("tool choice", func(t *testing.T) {
		req := func(choice *loom.ToolChoice) loom.ChatRequest {
			return loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}}, ToolChoice: choice, Reasoning: reasoning}
		}
		for mode, want := range map[loom.ToolChoiceMode]string{
			loom.ToolChoiceAuto: "auto", loom.ToolChoiceNone: "none", loom.ToolChoiceRequired: "required",
		} {
			if body := build(t, req(&loom.ToolChoice{Mode: mode})); body["tool_choice"] != want {
				t.Errorf("mode %q sent %#v", mode, body["tool_choice"])
			}
		}
		specific := build(t, req(&loom.ToolChoice{Mode: loom.ToolChoiceSpecific, Name: "lookup"}))
		if specific["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "lookup" {
			t.Errorf("specific choice sent %#v", specific["tool_choice"])
		}
	})

	t.Run("structured output", func(t *testing.T) {
		req := loom.ChatRequest{
			Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}},
			StructuredOutput: &loom.StructuredOutput{
				Mode: loom.StructuredOutputJSONSchema, Name: "Result Name",
				Schema: &loom.Schema{Type: "object"},
			},
			Reasoning: reasoning,
		}
		body := build(t, req)
		format := body["response_format"].(map[string]any)
		schema := format["json_schema"].(map[string]any)
		// The endpoint accepts only [a-zA-Z0-9_-] in a schema name, so the caller's name is
		// normalized rather than sent as it stands.
		if format["type"] != "json_schema" || schema["name"] != "Result_Name" || schema["strict"] != true {
			t.Errorf("structured output = %#v", format)
		}
	})

	t.Run("reasoning travels as an extra field", func(t *testing.T) {
		caps := loom.ModelCapabilities{Reasoning: loom.ReasoningSupportAlwaysOn, ReasoningEfforts: []loom.ReasoningEffort{"high"}}
		model, err := New(Config{APIKey: "k", ModelName: "x-ai/grok-4.3", Capabilities: &caps})
		if err != nil {
			t.Fatal(err)
		}
		request, err := model.buildRequest(loom.ChatRequest{
			Messages:  []loom.Message{{Role: loom.RoleUser, Content: "x"}},
			Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: "high"},
		})
		if err != nil {
			t.Fatal(err)
		}
		data, err := jsonv2.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := jsonv2.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		object, ok := body["reasoning"].(map[string]any)
		if !ok || object["enabled"] != true || object["effort"] != "high" {
			t.Fatalf("reasoning extra = %#v", body["reasoning"])
		}
		// The switch is not a first-class field of the request the SDK knows about.
		if _, present := body["thinking"]; present {
			t.Fatalf("a DeepSeek field leaked into an OpenRouter request: %#v", body["thinking"])
		}
	})
}

// A reasoning model that is not handed its own previous reasoning back cannot continue the
// chain it started, so an assistant turn carries it — in OpenRouter's field, which is the same
// one its responses use.
func TestBuildRequestCarriesReasoningBack(t *testing.T) {
	model, err := New(Config{APIKey: "k", ModelName: "x-ai/grok-4.3"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := model.buildRequest(loom.ChatRequest{
		Messages: []loom.Message{
			{Role: loom.RoleUser, Content: "hi"},
			{
				Role:             loom.RoleAssistant,
				Content:          "calling",
				ReasoningContent: "the chain so far",
				ToolCalls:        []loom.ToolCall{{ID: "c1", Name: "lookup", Arguments: `{}`}},
			},
			{Role: loom.RoleTool, Content: "result", ToolCallID: "c1"},
		},
		Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := jsonv2.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := jsonv2.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	assistant := body["messages"].([]any)[1].(map[string]any)
	if assistant["reasoning"] != "the chain so far" {
		t.Fatalf("assistant message = %#v", assistant)
	}
	// The other vendors' spelling is not this endpoint's.
	if _, present := assistant["reasoning_content"]; present {
		t.Fatalf("DeepSeek's field name leaked into an OpenRouter request: %#v", assistant)
	}
	// The request-level reasoning object is still there, and is a different thing: it asks
	// for reasoning, while the message field hands the previous reasoning back.
	if object, ok := body["reasoning"].(map[string]any); !ok || object["enabled"] != true {
		t.Fatalf("request-level reasoning = %#v", body["reasoning"])
	}
}
