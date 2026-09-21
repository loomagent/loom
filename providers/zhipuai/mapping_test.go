package zhipuai

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/loomagent/loom"
)

func TestNewRejectsEmptyCredentials(t *testing.T) {
	if _, err := New(Config{ModelName: "glm-5.3"}); err == nil || !strings.Contains(err.Error(), "APIKey") {
		t.Fatalf("error = %v", err)
	}
	if _, err := New(Config{APIKey: "k"}); err == nil || !strings.Contains(err.Error(), "ModelName") {
		t.Fatalf("error = %v", err)
	}
}

func TestModelIdentity(t *testing.T) {
	caps := loom.ModelCapabilities{StructuredOutput: loom.StructuredOutputJSONObject}
	model, err := New(Config{APIKey: "k", ModelName: "glm-5.3", Capabilities: &caps})
	if err != nil {
		t.Fatal(err)
	}
	if got := model.Name(); got != "zhipuai/glm-5.3" {
		t.Fatalf("Name() = %q", got)
	}
	if model.Capabilities().StructuredOutput != loom.StructuredOutputJSONObject {
		t.Fatalf("Capabilities() = %+v", model.Capabilities())
	}
}

// An unrecognised finish reason is an error rather than a pass-through: this endpoint uses
// its own set, so a value Loom does not know is one it cannot interpret.
func TestTranslateFinishReason(t *testing.T) {
	for raw, want := range map[string]loom.FinishReason{
		"stop":           loom.FinishReasonStop,
		"length":         loom.FinishReasonLength,
		"tool_calls":     loom.FinishReasonToolCalls,
		"function_call":  loom.FinishReasonToolCalls,
		"content_filter": loom.FinishReasonContentFilter,
		"":               "",
		"something_new":  loom.FinishReasonError,
	} {
		if got := translateFinishReason(raw); got != want {
			t.Errorf("translateFinishReason(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestTranslateReasoning(t *testing.T) {
	omitted, err := translateReasoning(loom.ResolvedReasoning{Send: loom.ReasoningSendOmit})
	if err != nil || len(omitted) != 0 {
		t.Fatalf("omit = %#v (%v)", omitted, err)
	}
	disabled, err := translateReasoning(loom.ResolvedReasoning{Send: loom.ReasoningSendDisabled})
	if err != nil || disabled["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatalf("disabled = %#v (%v)", disabled, err)
	}
	enabled, err := translateReasoning(loom.ResolvedReasoning{Send: loom.ReasoningSendEnabled, Effort: "max"})
	if err != nil || enabled["thinking"].(map[string]any)["type"] != "enabled" || enabled["reasoning_effort"] != "max" {
		t.Fatalf("enabled = %#v (%v)", enabled, err)
	}
	if _, present := omitted["reasoning_effort"]; present {
		t.Fatalf("an omitted switch carried an effort: %#v", omitted)
	}
	if _, err := translateReasoning(loom.ResolvedReasoning{Send: loom.ReasoningSend("other")}); err == nil {
		t.Fatal("an unknown decision must fail")
	}
}

// The header is the endpoint's own answer to "when may I come back", and this one bounds
// what a hostile or mistaken value can ask for.
func TestRetryAfter(t *testing.T) {
	soon := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	tests := []struct {
		name       string
		retryAfter string
		want       time.Duration
		exact      bool
	}{
		{name: "absent", want: 0},
		{name: "seconds", retryAfter: "7", want: 7 * time.Second, exact: true},
		{name: "zero", retryAfter: "0", want: 0},
		{name: "negative", retryAfter: "-5", want: 0},
		{name: "beyond a day is clamped", retryAfter: "999999", want: 86400 * time.Second, exact: true},
		{name: "http date", retryAfter: soon, want: 9 * time.Second},
		{name: "junk", retryAfter: "soon", want: 0},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			header := http.Header{}
			if testCase.retryAfter != "" {
				header.Set("Retry-After", testCase.retryAfter)
			}
			got := (classifier{}).RetryAfter(&APIError{StatusCode: http.StatusTooManyRequests, Header: header})
			if testCase.exact {
				if got != testCase.want {
					t.Fatalf("RetryAfter() = %s, want %s", got, testCase.want)
				}
				return
			}
			if testCase.want == 0 && got != 0 {
				t.Fatalf("RetryAfter() = %s, want none", got)
			}
			if testCase.want > 0 && (got < testCase.want || got > 11*time.Second) {
				t.Fatalf("RetryAfter() = %s, want about %s", got, testCase.want)
			}
		})
	}
	if got := (classifier{}).RetryAfter(errors.New("not an API error")); got != 0 {
		t.Fatalf("RetryAfter(network error) = %s", got)
	}
}

// What the endpoint receives, branch by branch.
func TestBuildRequestBranches(t *testing.T) {
	model, err := New(Config{APIKey: "k", ModelName: "glm-5.3"})
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

	t.Run("roles and tool history", func(t *testing.T) {
		body := build(t, loom.ChatRequest{Messages: []loom.Message{
			{Role: loom.RoleSystem, Content: "rules"},
			{Role: loom.RoleAssistant, Content: "calling", ReasoningContent: "why", ToolCalls: []loom.ToolCall{{ID: "c1", Name: "search", Arguments: `{}`}}},
			{Role: loom.RoleTool, Content: "result", ToolCallID: "c1"},
			{Role: loom.RoleUser, Content: "hi"},
		}, Reasoning: reasoning})
		messages := body["messages"].([]any)
		for index, want := range []string{"system", "assistant", "tool", "user"} {
			if got := messages[index].(map[string]any)["role"]; got != want {
				t.Errorf("message %d role = %v, want %q", index, got, want)
			}
		}
		// The reasoning belongs to the assistant turn it produced, because this endpoint
		// asks for it back on the next call.
		assistant := messages[1].(map[string]any)
		if assistant["reasoning_content"] != "why" || len(assistant["tool_calls"].([]any)) != 1 {
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

	t.Run("stop sequence limit", func(t *testing.T) {
		atLimit := build(t, loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}},
			Stop: []string{"A", "B", "C", "D"}, Reasoning: reasoning})
		if stops, ok := atLimit["stop"].([]any); !ok || len(stops) != 4 {
			t.Errorf("four stop sequences sent %#v", atLimit["stop"])
		}
		// The endpoint takes at most four, so the fifth is refused rather than dropped.
		_, err := model.buildRequest(loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}},
			Stop: []string{"A", "B", "C", "D", "E"}, Reasoning: reasoning})
		if !errors.Is(err, loom.ErrUnsupportedCapability) {
			t.Fatalf("error = %v, want ErrUnsupportedCapability", err)
		}
	})

	t.Run("structured output", func(t *testing.T) {
		body := build(t, loom.ChatRequest{
			Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}},
			StructuredOutput: &loom.StructuredOutput{
				Mode: loom.StructuredOutputJSONSchema, Name: "Result Name",
				Schema: &loom.Schema{Type: "object"},
			},
			Reasoning: reasoning,
		})
		format := body["response_format"].(map[string]any)
		schema := format["json_schema"].(map[string]any)
		// The endpoint accepts only [a-zA-Z0-9_-] in a schema name.
		if format["type"] != "json_schema" || schema["name"] != "Result_Name" || schema["strict"] != true {
			t.Errorf("structured output = %#v", format)
		}
	})

	t.Run("reasoning travels as an extra field", func(t *testing.T) {
		caps := loom.ModelCapabilities{Reasoning: loom.ReasoningSupportAlwaysOn, ReasoningEfforts: []loom.ReasoningEffort{"max"}}
		model, err := New(Config{APIKey: "k", ModelName: "glm-5.3", Capabilities: &caps})
		if err != nil {
			t.Fatal(err)
		}
		request, err := model.buildRequest(loom.ChatRequest{
			Messages:  []loom.Message{{Role: loom.RoleUser, Content: "x"}},
			Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: "max"},
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
		if thinking, ok := body["thinking"].(map[string]any); !ok || thinking["type"] != "enabled" {
			t.Fatalf("thinking extra = %#v", body["thinking"])
		}
		if body["reasoning_effort"] != "max" {
			t.Fatalf("reasoning_effort = %#v", body["reasoning_effort"])
		}
	})
}

// The probe reports what the adapter serializes, so it has to see the switch and the effort.
func TestReasoningRequestParameters(t *testing.T) {
	caps := loom.ModelCapabilities{Reasoning: loom.ReasoningSupportToggleable, ReasoningEfforts: []loom.ReasoningEffort{"max"}}
	model, err := New(Config{APIKey: "k", ModelName: "glm-5.3", Capabilities: &caps})
	if err != nil {
		t.Fatal(err)
	}
	params, err := model.ReasoningRequestParameters(loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: "max"})
	if err != nil {
		t.Fatal(err)
	}
	if thinking, ok := params["thinking"].(map[string]any); !ok || thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v", params["thinking"])
	}
	if params["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %#v", params["reasoning_effort"])
	}

	params, err = model.ReasoningRequestParameters(loom.Reasoning{Mode: loom.ReasoningModeDisabled})
	if err != nil {
		t.Fatal(err)
	}
	if thinking, ok := params["thinking"].(map[string]any); !ok || thinking["type"] != "disabled" {
		t.Fatalf("disabled thinking = %#v", params["thinking"])
	}
	if _, present := params["reasoning_effort"]; present {
		t.Fatalf("a disabled call sent an effort: %#v", params)
	}
}
