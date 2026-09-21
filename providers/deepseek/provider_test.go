package deepseek

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/loomagent/loom"
)

func TestClassifierTreats503AsTransientAndReadsRetryAfter(t *testing.T) {
	serviceUnavailable := &openai.Error{StatusCode: http.StatusServiceUnavailable}
	if got := (classifier{}).ClassifyError(serviceUnavailable); got != loom.ErrorClassTransient {
		t.Fatalf("503 class = %s, want transient", got)
	}
	if !(classifier{}).IsServiceUnavailable(serviceUnavailable) {
		t.Fatal("503 must open the shared service-unavailable circuit")
	}
	rateLimited := &openai.Error{
		StatusCode: http.StatusTooManyRequests,
		Response:   &http.Response{Header: http.Header{"Retry-After": []string{"7"}}},
	}
	if got := (classifier{}).ClassifyError(rateLimited); got != loom.ErrorClassRateLimit {
		t.Fatalf("429 class = %s, want rate_limit", got)
	}
	if got := (classifier{}).RetryAfter(rateLimited); got != 7*time.Second {
		t.Fatalf("RetryAfter = %s, want 7s", got)
	}
}

// TestBuildRequestReasoningModeRequired is the provider-level backstop for the
// required-Mode contract: a caller that forgets Reasoning.Mode fails while the
// request is being built, and nothing is sent.
func TestBuildRequestReasoningModeRequired(t *testing.T) {
	m, err := New(Config{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.buildRequest(loom.ChatRequest{
		Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
		// Reasoning is deliberately omitted
	})
	if err == nil {
		t.Fatal("a missing Reasoning.Mode must fail")
	}
	if !strings.Contains(err.Error(), "is required") {
		t.Fatalf("error %q does not say the mode is required", err.Error())
	}
}

// TestBuildRequestReasoningModeExplicit checks that an explicit enabled or disabled puts
// DeepSeek's own thinking object in the request, injected through the SDK's extra fields.
func TestBuildRequestReasoningModeExplicit(t *testing.T) {
	m, err := New(Config{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, mode := range []loom.ReasoningMode{loom.ReasoningModeEnabled, loom.ReasoningModeDisabled} {
		req, err := m.buildRequest(loom.ChatRequest{
			Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
			Reasoning: loom.Reasoning{Mode: mode},
		})
		if err != nil {
			t.Fatalf("mode=%s buildRequest: %v", mode, err)
		}
		data, err := jsonv2.Marshal(req)
		if err != nil {
			t.Fatalf("mode=%s marshal: %v", mode, err)
		}
		var body struct {
			Thinking *struct {
				Type string `json:"type"`
			} `json:"thinking"`
		}
		if err := jsonv2.Unmarshal(data, &body); err != nil {
			t.Fatalf("mode=%s unmarshal: %v", mode, err)
		}
		if body.Thinking == nil {
			t.Fatalf("mode=%s expected an explicit thinking field, it is missing: %s", mode, data)
		}
		if body.Thinking.Type != string(mode) {
			t.Fatalf("mode=%s thinking.type = %q", mode, body.Thinking.Type)
		}
	}
}

func TestNormalizeDeepSeekContentExistsRisk(t *testing.T) {
	apiErr := &openai.Error{
		StatusCode: 400,
		Message:    "Content Exists Risk",
	}
	err := normalizeDeepSeekError(apiErr)

	if !errors.Is(err, loom.ErrSensitiveContentRisk) {
		t.Fatalf("normalized error = %v, want ErrSensitiveContentRisk", err)
	}
	var got *openai.Error
	if !errors.As(err, &got) || got != apiErr {
		t.Fatalf("normalized error does not preserve original APIError")
	}
	if (classifier{}).ClassifyError(err) != loom.ErrorClassPermanent {
		t.Fatalf("classifier = %v, want permanent", (classifier{}).ClassifyError(err))
	}
}

func TestNormalizeDeepSeekContentExistsRiskRequiresOfficial400(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "same message different status",
			err:  &openai.Error{StatusCode: 500, Message: "Content Exists Risk"},
		},
		{
			name: "ordinary bad request",
			err:  &openai.Error{StatusCode: 400, Message: "invalid request"},
		},
		{
			name: "plain string is not enough",
			err:  errors.New("Content Exists Risk"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := normalizeDeepSeekError(tt.err)
			if errors.Is(err, loom.ErrSensitiveContentRisk) {
				t.Fatalf("normalized error = %v, did not expect ErrSensitiveContentRisk", err)
			}
		})
	}
}
