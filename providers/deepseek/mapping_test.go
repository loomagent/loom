package deepseek

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/loomagent/loom"
)

// testModel points a model at an in-memory test server. The API key is a placeholder: the
// fake server checks the header it receives, not the value.
func testModel(t *testing.T, handler http.HandlerFunc) *Model {
	t.Helper()
	server := httptest.NewTestServer(t, handler)
	model, err := New(Config{
		APIKey:     "test",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		ModelName:  "deepseek-chat",
		Retry:      &loom.RetryConfig{Mode: loom.RetryModeDisabled},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestModelIdentity(t *testing.T) {
	caps := loom.ModelCapabilities{StructuredOutput: loom.StructuredOutputJSONSchema}
	model, err := New(Config{APIKey: "k", ModelName: ModelV4Pro, Capabilities: &caps})
	if err != nil {
		t.Fatal(err)
	}
	if got := model.Name(); got != "deepseek/"+ModelV4Pro {
		t.Fatalf("Name() = %q", got)
	}
	if model.Capabilities().StructuredOutput != loom.StructuredOutputJSONSchema {
		t.Fatalf("Capabilities() = %+v", model.Capabilities())
	}
	// An unnamed model falls back to the default alias rather than an empty name.
	if fallback, err := New(Config{APIKey: "k"}); err != nil {
		t.Fatal(err)
	} else if fallback.Name() != "deepseek/"+ModelV4Flash {
		t.Fatalf("default Name() = %q", fallback.Name())
	}
}

// apiError builds an SDK error with the response the classifier inspects.
func apiError(status int, retryAfter string) error {
	header := http.Header{}
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	response := &http.Response{StatusCode: status, Header: header, Status: http.StatusText(status)}
	return &openai.Error{
		StatusCode: status,
		Request:    httptest.NewRequest(http.MethodPost, "https://api.deepseek.com/chat/completions", nil),
		Response:   response,
	}
}

// The classification decides whether a turn retries or gives up, which is the difference
// between a transient hiccup and burning a budget on an error that cannot change.
func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want loom.ErrorClass
	}{
		{name: "nil", err: nil, want: loom.ErrorClassUnknown},
		{name: "cancelled", err: context.Canceled, want: loom.ErrorClassPermanent},
		{name: "deadline", err: context.DeadlineExceeded, want: loom.ErrorClassPermanent},
		{name: "sensitive content", err: loom.ErrSensitiveContentRisk, want: loom.ErrorClassPermanent},
		{name: "wrapped sensitive content", err: errors.Join(errors.New("x"), loom.ErrSensitiveContentRisk), want: loom.ErrorClassPermanent},
		{name: "rate limit", err: apiError(http.StatusTooManyRequests, ""), want: loom.ErrorClassRateLimit},
		{name: "bad request", err: apiError(http.StatusBadRequest, ""), want: loom.ErrorClassPermanent},
		{name: "unauthorized", err: apiError(http.StatusUnauthorized, ""), want: loom.ErrorClassPermanent},
		{name: "payment required", err: apiError(http.StatusPaymentRequired, ""), want: loom.ErrorClassPermanent},
		{name: "forbidden", err: apiError(http.StatusForbidden, ""), want: loom.ErrorClassPermanent},
		{name: "missing model", err: apiError(http.StatusNotFound, ""), want: loom.ErrorClassPermanent},
		{name: "other client error", err: apiError(http.StatusTeapot, ""), want: loom.ErrorClassPermanent},
		{name: "server error", err: apiError(http.StatusInternalServerError, ""), want: loom.ErrorClassTransient},
		{name: "unavailable", err: apiError(http.StatusServiceUnavailable, ""), want: loom.ErrorClassTransient},
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

// Retry-After is the endpoint's own answer to "when may I come back", so both forms the
// header allows are read, and anything else means "no instruction".
func TestRetryAfter(t *testing.T) {
	soon := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	tests := []struct {
		name       string
		err        error
		retryAfter string
		want       time.Duration
		exact      bool
	}{
		{name: "no header", err: apiError(http.StatusTooManyRequests, ""), want: 0},
		{name: "seconds", err: apiError(http.StatusTooManyRequests, "7"), want: 7 * time.Second, exact: true},
		{name: "zero seconds", err: apiError(http.StatusTooManyRequests, "0"), want: 0},
		{name: "negative seconds", err: apiError(http.StatusTooManyRequests, "-5"), want: 0},
		{name: "http date", err: apiError(http.StatusTooManyRequests, soon), want: 9 * time.Second, exact: false},
		{name: "junk", err: apiError(http.StatusTooManyRequests, "soon"), want: 0},
		{name: "not an sdk error", err: errors.New("network"), want: 0},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := (classifier{}).RetryAfter(testCase.err)
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
}

func TestIsServiceUnavailable(t *testing.T) {
	if !(classifier{}).IsServiceUnavailable(apiError(http.StatusServiceUnavailable, "")) {
		t.Fatal("503 must open the shared circuit")
	}
	if (classifier{}).IsServiceUnavailable(apiError(http.StatusInternalServerError, "")) ||
		(classifier{}).IsServiceUnavailable(errors.New("x")) {
		t.Fatal("only 503 opens the shared circuit")
	}
}

// An unrecognised finish reason passes through instead of being dropped, so a caller still
// sees what the provider said.
func TestTranslateFinishReason(t *testing.T) {
	for raw, want := range map[string]loom.FinishReason{
		"stop":                         loom.FinishReasonStop,
		"length":                       loom.FinishReasonLength,
		"content_filter":               loom.FinishReasonContentFilter,
		"tool_calls":                   loom.FinishReasonToolCalls,
		"function_call":                loom.FinishReasonToolCalls,
		"insufficient_system_resource": loom.FinishReasonError,
		"":                             "",
		"something_new":                loom.FinishReason("something_new"),
	} {
		if got := translateFinishReason(raw); got != want {
			t.Errorf("translateFinishReason(%q) = %q, want %q", raw, got, want)
		}
	}
}

// What the endpoint receives, branch by branch: the roles, the stop sequences, the tool
// choice, and the response format.
func TestBuildRequestBranches(t *testing.T) {
	model, err := New(Config{APIKey: "k", ModelName: "deepseek-chat"})
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
			{Role: loom.RoleUser, Content: "hi"},
			{Role: loom.RoleAssistant, Content: "calling", Name: "assistant", ToolCalls: []loom.ToolCall{{ID: "c1", Name: "search", Arguments: `{}`}}},
			{Role: loom.RoleTool, Content: "result", ToolCallID: "c1"},
		}, Reasoning: reasoning})
		messages := body["messages"].([]any)
		if len(messages) != 4 {
			t.Fatalf("messages = %d", len(messages))
		}
		for index, want := range []string{"system", "user", "assistant", "tool"} {
			if got := messages[index].(map[string]any)["role"]; got != want {
				t.Errorf("message %d role = %v, want %q", index, got, want)
			}
		}
		assistant := messages[2].(map[string]any)
		if assistant["name"] != "assistant" || len(assistant["tool_calls"].([]any)) != 1 {
			t.Errorf("assistant = %#v", assistant)
		}
		if tool := messages[3].(map[string]any); tool["tool_call_id"] != "c1" || tool["content"] != "result" {
			t.Errorf("tool = %#v", tool)
		}
	})

	t.Run("stop sequences", func(t *testing.T) {
		if body := build(t, loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}}, Reasoning: reasoning}); body["stop"] != nil {
			t.Errorf("no stop sequences sent %v", body["stop"])
		}
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
		if body := build(t, req(nil)); body["tool_choice"] != nil {
			t.Errorf("no tool choice sent %v", body["tool_choice"])
		}
		for mode, want := range map[loom.ToolChoiceMode]string{
			loom.ToolChoiceAuto: "auto", loom.ToolChoiceNone: "none", loom.ToolChoiceRequired: "required",
		} {
			if body := build(t, req(&loom.ToolChoice{Mode: mode})); body["tool_choice"] != want {
				t.Errorf("mode %q sent %#v", mode, body["tool_choice"])
			}
		}
		specific := build(t, req(&loom.ToolChoice{Mode: loom.ToolChoiceSpecific, Name: "lookup"}))
		choice := specific["tool_choice"].(map[string]any)
		if choice["type"] != "function" || choice["function"].(map[string]any)["name"] != "lookup" {
			t.Errorf("specific choice sent %#v", choice)
		}
		if body := build(t, req(&loom.ToolChoice{Mode: loom.ToolChoiceMode("unknown")})); body["tool_choice"] != nil {
			t.Errorf("unknown mode sent %#v", body["tool_choice"])
		}
	})

	t.Run("response format", func(t *testing.T) {
		base := func() loom.ChatRequest {
			return loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}}, Reasoning: reasoning}
		}
		jsonObject := base()
		jsonObject.ResponseFormat = loom.ResponseFormatJSONObject
		if body := build(t, jsonObject); body["response_format"].(map[string]any)["type"] != "json_object" {
			t.Errorf("response format = %#v", body["response_format"])
		}

		structured := base()
		structured.StructuredOutput = &loom.StructuredOutput{
			Mode: loom.StructuredOutputJSONSchema, Name: "Result Name",
			Description: "what it is", Schema: &loom.Schema{Type: "object"},
		}
		body := build(t, structured)
		format := body["response_format"].(map[string]any)
		schema := format["json_schema"].(map[string]any)
		if format["type"] != "json_schema" || schema["name"] != "Result_Name" ||
			schema["description"] != "what it is" || schema["strict"] != true {
			t.Errorf("structured output = %#v", format)
		}

		object := base()
		object.StructuredOutput = &loom.StructuredOutput{Mode: loom.StructuredOutputJSONObject}
		if body := build(t, object); body["response_format"].(map[string]any)["type"] != "json_object" {
			t.Errorf("structured json_object = %#v", body["response_format"])
		}

		unsupported := base()
		unsupported.StructuredOutput = &loom.StructuredOutput{Mode: loom.StructuredOutputUnsupported}
		if body := build(t, unsupported); body["response_format"] != nil {
			t.Errorf("unsupported sent %#v", body["response_format"])
		}
	})

	t.Run("rejected modes", func(t *testing.T) {
		none := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}}, Reasoning: reasoning,
			StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputNone}}
		if _, err := model.buildRequest(none); err == nil || !strings.Contains(err.Error(), "may not be") {
			t.Errorf("error = %v", err)
		}
		unknown := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}}, Reasoning: reasoning,
			StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputMode("other")}}
		// The capability check rejects an unknown mode before the provider sees it, so the
		// provider's own branch stays defensive.
		if _, err := model.buildRequest(unknown); err == nil || !strings.Contains(err.Error(), `unknown StructuredOutput.Mode "other"`) {
			t.Errorf("error = %v", err)
		}
		missingSchema := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "x"}}, Reasoning: reasoning,
			StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputJSONSchema, Name: "s"}}
		if _, err := model.buildRequest(missingSchema); err == nil || !strings.Contains(err.Error(), "has no schema") {
			t.Errorf("error = %v", err)
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

	t.Run("optional fields", func(t *testing.T) {
		body := build(t, loom.ChatRequest{
			Messages:    []loom.Message{{Role: loom.RoleUser, Content: "x"}},
			Temperature: func() *float64 { v := 0.5; return &v }(),
			TopP:        func() *float64 { v := 0.9; return &v }(),
			MaxTokens:   func() *int { v := 128; return &v }(),
			Reasoning:   reasoning,
		})
		if body["temperature"] != 0.5 || body["top_p"] != 0.9 || body["max_tokens"] != float64(128) {
			t.Fatalf("optional fields = %#v", body)
		}
	})
}

// The probe reports what the adapter serializes, so it has to see the reasoning switch and
// the effort the request carries.
func TestReasoningRequestParameters(t *testing.T) {
	caps := loom.ModelCapabilities{Reasoning: loom.ReasoningSupportToggleable, ReasoningEfforts: []loom.ReasoningEffort{"high"}}
	model, err := New(Config{APIKey: "k", ModelName: "deepseek-chat", Capabilities: &caps})
	if err != nil {
		t.Fatal(err)
	}
	params, err := model.ReasoningRequestParameters(loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	thinking, ok := params["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v", params["thinking"])
	}
	if params["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v", params["reasoning_effort"])
	}
	// Disabling sends the switch without an effort.
	params, err = model.ReasoningRequestParameters(loom.Reasoning{Mode: loom.ReasoningModeDisabled})
	if err != nil {
		t.Fatal(err)
	}
	if disabled, ok := params["thinking"].(map[string]any); !ok || disabled["type"] != "disabled" {
		t.Fatalf("disabled thinking = %#v", params["thinking"])
	}
	if _, present := params["reasoning_effort"]; present {
		t.Fatalf("a disabled call sent an effort: %#v", params)
	}
}

// The reasoning output arrives as an extra field, and only a string is reasoning: a null or
// an object is not, and neither invents an error.
func TestChatReadsReasoningContentOnlyFromAString(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		payload string
		want    string
	}{
		{name: "string", payload: `"thinking out loud"`, want: "thinking out loud"},
		{name: "null", payload: `null`, want: ""},
		{name: "object", payload: `{"steps":[]}`, want: ""},
		{name: "absent", payload: ``, want: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reasoning := ""
			if testCase.payload != "" {
				reasoning = `,"reasoning_content":` + testCase.payload
			}
			model := testModel(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"deepseek-chat","choices":[{"message":{"content":"answer"` + reasoning + `},"finish_reason":"stop"}]}`))
			})
			response, err := model.Chat(t.Context(), loom.ChatRequest{
				Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
				Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.ReasoningContent != testCase.want || response.Content != "answer" {
				t.Fatalf("response = %+v, want reasoning %q", response, testCase.want)
			}
		})
	}
}

// The endpoint requires an assistant turn's reasoning back on the next call of a thinking
// tool loop; without it the API answers with "The reasoning_content in the thinking mode must
// be passed back to the API".
func TestBuildRequestCarriesReasoningBack(t *testing.T) {
	model, err := New(Config{APIKey: "k", ModelName: "deepseek-reasoner"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := model.buildRequest(loom.ChatRequest{
		Messages: []loom.Message{
			{Role: loom.RoleUser, Content: "hi"},
			{
				Role:             loom.RoleAssistant,
				Content:          "calling",
				ReasoningContent: "the reasoning that must come back",
				ToolCalls:        []loom.ToolCall{{ID: "c1", Name: "lookup", Arguments: `{}`}},
			},
			{Role: loom.RoleTool, Content: "result", ToolCallID: "c1"},
		},
		Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled},
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
	if assistant["reasoning_content"] != "the reasoning that must come back" {
		t.Fatalf("assistant message = %#v", assistant)
	}
}
