package deepseek_test

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/deepseek"
	"github.com/openai/openai-go/v3"
)

// This is the unmodified response captured from the dev AgentGuard request.
const capturedContentRisk = `{"error":{"message":"Content Exists Risk (request_id: 6f0ff783-d275-4c95-913e-aa20f30a3d8d)","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`

func Test_user_receives_content_risk_for_official_rejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"user receives the captured rejection with a request ID", capturedContentRisk},
		{"user receives the original rejection without a request ID", `{"error":{"message":"Content Exists Risk","type":"invalid_request_error","code":"invalid_request_error"}}`},
		{"user receives the rejection with normalized casing and whitespace", `{"error":{"message":"  content exists risk (request_id: trace-1)  ","type":"invalid_request_error","code":"invalid_request_error"}}`},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					// Given an official HTTP 400 content rejection, including its trace header.
					model := rejectingModel(t, http.StatusBadRequest, tc.body)
					// When the user calls the real provider through its HTTP transport.
					err := callModel(t, model, streaming)
					// Then callers can distinguish content risk without losing diagnostics.
					if !errors.Is(err, loom.ErrSensitiveContentRisk) {
						t.Fatalf("expected content-risk rejection, got %v", err)
					}
					assertUpstreamError(t, err, http.StatusBadRequest)
					if class, ok := loom.ErrorClassOf(err); !ok || class != loom.ErrorClassPermanent {
						t.Fatalf("content risk must be permanent, got %s (%t)", class, ok)
					}
					failure, ok := errors.AsType[*loom.ClassifiedError](err)
					if !ok || failure.Attempts != 1 {
						t.Fatalf("content rejection must not consume additional attempts: %v", err)
					}
				})
			})
		}
	}
}

func Test_user_does_not_receive_content_risk_for_unrelated_failures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"user receives a parameter error", 400, `{"error":{"message":"invalid messages","code":"invalid_request_error"}}`},
		{"user receives a parameter error quoting the risk phrase", 400, `{"error":{"message":"invalid parameter: Content Exists Risk","code":"invalid_request_error"}}`},
		{"user receives an unrelated phrase extension", 400, `{"error":{"message":"Content Exists Risk policy configuration is invalid","code":"invalid_request_error"}}`},
		{"user receives an empty trace suffix", 400, `{"error":{"message":"Content Exists Risk (request_id: )","code":"invalid_request_error"}}`},
		{"user receives unrelated parenthetical text", 400, `{"error":{"message":"Content Exists Risk (unsupported parameter)","code":"invalid_request_error"}}`},
		{"user receives an authentication error", 401, capturedContentRisk},
		{"user receives an insufficient-balance error", 402, capturedContentRisk},
		{"user receives a permission error", 403, capturedContentRisk},
		{"user receives a server failure with the same phrase", 500, capturedContentRisk},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					// Given a non-moderation response, even if it mentions the phrase.
					model := rejectingModel(t, tc.status, tc.body)
					// When the user calls the real provider.
					err := callModel(t, model, streaming)
					// Then the original typed failure remains, without a content-risk label.
					if errors.Is(err, loom.ErrSensitiveContentRisk) {
						t.Fatalf("unrelated failure was misclassified: %v", err)
					}
					assertUpstreamError(t, err, tc.status)
				})
			})
		}
	}
}

func rejectingModel(t *testing.T, status int, body string) *deepseek.Model {
	t.Helper()
	var fixture map[string]any
	if err := jsonv2.Unmarshal([]byte(body), &fixture); err != nil {
		t.Fatalf("invalid strict-JSON response fixture: %v", err)
	}
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "trace-1")
		w.WriteHeader(status)
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write rejection: %v", err)
		}
	}))
	model, err := deepseek.New(deepseek.Config{
		APIKey: "fixture", ModelName: "fixture-model", BaseURL: server.URL, HTTPClient: server.Client(),
		Retry: &loom.RetryConfig{Mode: loom.RetryModeFinite, MaxRetries: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func callModel(t *testing.T, model loom.ChatModel, streaming bool) error {
	t.Helper()
	req := loom.ChatRequest{
		Messages:       []loom.Message{{Role: loom.RoleUser, Content: "classify this input as JSON"}},
		Reasoning:      loom.Reasoning{Mode: loom.ReasoningModeDisabled},
		ResponseFormat: loom.ResponseFormatJSONObject,
	}
	if !streaming {
		_, err := model.Chat(t.Context(), req)
		return err
	}
	stream, err := model.Stream(t.Context(), req)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	_, err = stream.Recv()
	return err
}

func assertUpstreamError(t *testing.T, err error, status int) {
	t.Helper()
	apiErr, ok := errors.AsType[*openai.Error](err)
	if !ok || apiErr.StatusCode != status || apiErr.Code != "invalid_request_error" {
		t.Fatalf("upstream status/code were lost: %v", err)
	}
	if apiErr.Response == nil || apiErr.Response.Header.Get("X-Request-ID") != "trace-1" {
		t.Fatalf("upstream request ID was lost: %v", err)
	}
}
