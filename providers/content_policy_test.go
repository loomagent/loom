package providers_test

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/ark"
	"github.com/loomagent/loom/providers/deepseek"
	"github.com/loomagent/loom/providers/openrouter"
	"github.com/loomagent/loom/providers/zhipuai"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

type policyFixture struct {
	provider string
	status   int
	body     string
	risk     bool
}

// Fixtures follow the official chat-completions contracts linked in
// docs/provider-content-policy.md; none contains a real credential or prompt.
func policyFixtures() []policyFixture {
	fixtures := []policyFixture{
		{"deepseek", 400, `{"error":{"message":"Content Exists Risk (request_id: trace-1)","type":"invalid_request_error","code":"invalid_request_error"}}`, true},
		{"deepseek", 400, `{"error":{"message":"Content Exists Risk","type":"invalid_request_error","code":"invalid_request_error"}}`, true},
		{"deepseek", 400, `{"error":{"message":"invalid parameter quoting Content Exists Risk","code":"invalid_request_error"}}`, false},
		{"deepseek", 400, `{"error":{"message":"Content Exists Risk (unsupported parameter)","code":"invalid_request_error"}}`, false},
		{"deepseek", 403, `{"error":{"message":"Content Exists Risk","code":"permission_denied"}}`, false},
		{"deepseek", 400, `{"error":{"message":"Content Exists Risk","type":"permission_error","code":"permission_denied"}}`, false},
		{"zhipuai", 400, `{"error":{"code":"1301","message":"input or output rejected","request_id":"trace-1"}}`, true},
		{"zhipuai", 400, `{"error":{"code":1301,"message":"input or output rejected","request_id":"trace-1"}}`, true},
		{"zhipuai", 400, `{"error":{"code":"1214","message":"invalid parameter quoting sensitive content"}}`, false},
		{"zhipuai", 403, `{"error":{"code":"1220","message":"permission denied"}}`, false},
		{"zhipuai", 429, `{"error":{"code":"1113","message":"insufficient balance"}}`, false},
		{"ark", 400, `{"error":{"code":"InvalidParameter","message":"invalid parameter quoting SensitiveContentDetected","type":"BadRequest"}}`, false},
		{"ark", 400, `{"error":{"code":"ContentSecurityDetectionError","message":"moderation service unavailable","type":"BadRequest"}}`, false},
		{"ark", 400, `{"error":{"code":"SensitiveContentDetectedUnexpected","message":"unrelated code","type":"BadRequest"}}`, false},
		{"ark", 403, `{"error":{"code":"AccessDenied","message":"permission denied","type":"Forbidden"}}`, false},
		{"ark", 400, `{"error":{"code":"SensitiveContentDetected.","message":"invalid subcategory","type":"BadRequest"}}`, false},
		{"openrouter", 403, `{"error":{"code":403,"message":"rejected","metadata":{"error_type":"content_policy_violation","provider_code":"SAFETY"}}}`, true},
		{"openrouter", 403, `{"error":{"code":403,"message":"provider refused","metadata":{"error_type":"refusal"}}}`, true},
		{"openrouter", 403, `{"error":{"code":403,"message":"rejected","metadata":{"reasons":["fixture-policy"],"flagged_input":"fixture","provider_name":"fixture-provider","model_slug":"fixture-model"}}}`, true},
		{"openrouter", 400, `{"error":{"code":400,"message":"invalid parameter quoting content_policy_violation","metadata":{"error_type":"invalid_request"}}}`, false},
		{"openrouter", 403, `{"error":{"code":403,"message":"permission denied","metadata":{"error_type":"permission_denied","reasons":["permission"]}}}`, false},
		{"openrouter", 403, `{"error":{"code":403,"message":"budget limit","metadata":{"limit_source":"api_key"}}}`, false},
		{"openrouter", 403, `{"error":{"code":403,"message":"unrelated metadata","metadata":{"reasons":["permission"]}}}`, false},
		{"openrouter", 403, `{"error":{"code":403,"message":"content_policy_violation"}}`, false},
		{"openrouter", 403, `{"error":{"code":403,"message":"permission denied","metadata":{"error_type":"permission_denied","reasons":["permission"],"flagged_input":"fixture","provider_name":"fixture","model_slug":"fixture"}}}`, false},
	}
	for _, code := range []string{
		"SensitiveContentDetected", "SensitiveContentDetected.SevereViolation", "SensitiveContentDetected.Violence",
		"InputTextSensitiveContentDetected", "InputTextSensitiveContentDetected.PolicyViolation", "OutputTextSensitiveContentDetected",
		"InputImageSensitiveContentDetected.PrivacyInformation", "InputVideoSensitiveContentDetected", "InputAudioSensitiveContentDetected",
		"OutputImageSensitiveContentDetected.DeepFake", "OutputVideoSensitiveContentDetected.PolicyViolation", "OutputAudioSensitiveContentDetected",
		"InputTextRiskDetection", "OutputTextRiskDetection", "InputImageRiskDetection", "OutputImageRiskDetection",
	} {
		fixtures = append(fixtures, policyFixture{"ark", 400, fmt.Sprintf(`{"error":{"code":%q,"message":"rejected","type":"BadRequest","request_id":"trace-1"}}`, code), true})
	}
	return fixtures
}

func Test_user_receives_Ark_SSE_rejection_regardless_of_JSON_field_order(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given a valid SSE error envelope whose error object is not the first field.
		model := policyModel(t, "ark", 200, `{"id":"generation-1", "error": {"code":"OutputTextSensitiveContentDetected","type":"BadRequest","message":"rejected"}}`, true, true)
		// When the user consumes the stream, JSON object ordering has no semantic meaning.
		content, err := consumePolicyModel(t, model, true)
		// Then the output refusal is preserved after the already-visible answer.
		assertPolicyError(t, err, true)
		if content != "partial answer" {
			t.Fatalf("partial output changed: %q", content)
		}
		if e, ok := errors.AsType[*arkmodel.APIError](err); !ok || e.RequestId != "trace-1" {
			t.Fatalf("lost trace ID: %v", err)
		}
	})
}

func Test_user_never_receives_OpenRouter_failed_finish_as_success(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("user streaming=%t", stream), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given a failed finish without enough structured evidence to call it moderation.
				field := "message"
				if stream {
					field = "delta"
				}
				model := policyModel(t, "openrouter", 200, fmt.Sprintf(`{"choices":[{"index":0,%q:{"content":"failed partial answer"},"finish_reason":"error"}]}`, field), stream, false)
				// When the user consumes the real adapter, the failure cannot be a normal stop.
				_, err := consumePolicyModel(t, model, stream)
				// Then it remains an operational failure, not a successful or moderation result.
				assertPolicyError(t, err, false)
			})
		})
	}
}

func Test_user_distinguishes_moderation_from_operational_HTTP_errors(t *testing.T) {
	for i, fixture := range policyFixtures() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("user %s fixture=%d streaming=%t", fixture.provider, i, stream), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					// Given an official HTTP refusal or an unrelated parameter/permission failure.
					model := policyModel(t, fixture.provider, fixture.status, fixture.body, false, false)
					// When the user calls the actual adapter, including stream establishment.
					_, err := consumePolicyModel(t, model, stream)
					// Then only explicit moderation becomes permanent content risk; retain SDK diagnostics.
					assertPolicyError(t, err, fixture.risk)
					if fixture.provider == "ark" {
						e, ok := errors.AsType[*arkmodel.APIError](err)
						if !ok || e.HTTPStatusCode != fixture.status || e.RequestId != "trace-1" {
							t.Fatalf("lost Ark status/request ID: %v", err)
						}
					} else {
						e, ok := errors.AsType[*openai.Error](err)
						if !ok || e.StatusCode != fixture.status || e.Response.Header.Get("X-Request-ID") != "trace-1" {
							t.Fatalf("lost SDK status/request ID: %v", err)
						}
						if fixture.provider == "openrouter" {
							policy, ok := errors.AsType[*openrouter.APIError](err)
							if !ok || policy.RequestID != "generation-1" || policy.RawJSON() == "" {
								t.Fatalf("lost structured OpenRouter diagnostics: %v", err)
							}
						}
					}
				})
			})
		}
	}
}

func Test_user_receives_moderation_or_operational_error_inside_SSE(t *testing.T) {
	for i, fixture := range policyFixtures() {
		// Zhipu documents finish_reason for inference termination, tested below.
		// Its compatible error envelope is also exercised as an error, never success.
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("user %s fixture=%d partial=%t", fixture.provider, i, partial), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					// Given HTTP 200 with a structured SSE error, optionally after visible output.
					model := policyModel(t, fixture.provider, 200, fixture.body, true, partial)
					// When the user consumes the actual provider stream.
					content, err := consumePolicyModel(t, model, true)
					// Then error events never become successful completion, and delivered output is not replayed.
					assertPolicyError(t, err, fixture.risk)
					if partial && content != "partial answer" {
						t.Fatalf("partial answer changed or replayed: %q", content)
					}
					if fixture.provider == "ark" {
						if e, ok := errors.AsType[*arkmodel.APIError](err); !ok || e.RequestId != "trace-1" {
							t.Fatalf("lost SSE Ark diagnostics: %v", err)
						}
					} else if _, ok := errors.AsType[*ssestream.StreamError](err); !ok {
						t.Fatalf("lost original SSE event: %v", err)
					}
					if fixture.provider == "openrouter" {
						policy, ok := errors.AsType[*openrouter.APIError](err)
						if !ok || policy.StatusCode != 200 || policy.RequestID != "generation-1" || policy.RawJSON() == "" {
							t.Fatalf("lost SSE generation diagnostics: %v", err)
						}
					}
				})
			})
		}
	}
}

func Test_user_receives_OpenRouter_errors_in_HTTP_200_completion_bodies(t *testing.T) {
	for _, fixture := range policyFixtures() {
		if fixture.provider != "openrouter" {
			continue
		}
		for _, inChoice := range []bool{false, true} {
			t.Run(fmt.Sprintf("user risk=%t choice=%t body=%s", fixture.risk, inChoice, fixture.body), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					// Given OpenRouter has committed HTTP 200 before generation fails.
					body := fixture.body
					if inChoice {
						body = `{"id":"generation-1","choices":[{"index":0,"message":{"role":"assistant","content":"partial answer"},"finish_reason":"error",` + strings.TrimPrefix(fixture.body, "{") + `]}`
					}
					model := policyModel(t, fixture.provider, 200, body, false, false)
					// When the user requests a non-streaming completion.
					content, err := consumePolicyModel(t, model, false)
					// Then the refusal is classified, and partial content is not returned as a successful answer.
					assertPolicyError(t, err, fixture.risk)
					if content != "" {
						t.Fatal("failed completion exposed a successful answer")
					}
				})
			})
		}
	}
}

func Test_user_sees_content_filter_terminal_after_output_moderation(t *testing.T) {
	for _, provider := range []string{"deepseek", "zhipuai", "ark", "openrouter"} {
		for _, finish := range []string{"content_filter", "sensitive"} {
			if finish == "sensitive" && provider != "zhipuai" {
				continue
			}
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("user %s finish=%s stream=%t", provider, finish, stream), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						// Given a provider's explicit output-moderation finish reason.
						field := "message"
						if stream {
							field = "delta"
						}
						body := fmt.Sprintf(`{"model":"fixture-model","choices":[{"index":0,%q:{"role":"assistant","content":""},"finish_reason":%q}]}`, field, finish)
						model := policyModel(t, provider, 200, body, stream, stream)
						// When the user runs a real Loom turn through the actual adapter.
						turn, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
							if stream {
								_, err := loom.StreamLLMToFinalAnswer(ctx, w, "final answer", model, policyRequest())
								return err
							}
							response, err := model.Chat(ctx, policyRequest())
							if err != nil {
								return err
							}
							if response.FinishReason == loom.FinishReasonContentFilter {
								return loom.ErrContentFilter
							}
							return w.FinalAnswer(ctx, response.Content)
						}, loom.RunOptions{ConversationID: "fixture-conversation", Input: loom.UserMessage{Text: "fixture task"}})
						// Then the visible terminal is failed/content_filter, with no committed final answer.
						if err == nil || turn.Status != loom.TurnStatusFailed || turn.CloseReason.Code != loom.CloseCodeContentFilter {
							t.Fatalf("wrong moderation terminal: turn=%+v err=%v", turn, err)
						}
					})
				})
			}
		}
	}
}

func assertPolicyError(t *testing.T, err error, risk bool) {
	t.Helper()
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, loom.ErrSensitiveContentRisk) != risk {
		t.Fatalf("risk=%t was not preserved: %v", risk, err)
	}
	if risk {
		if loom.FailureCloseCode(err) != loom.CloseCodeContentFilter {
			t.Fatalf("wrong terminal classification: %v", err)
		}
		if classified, ok := errors.AsType[*loom.ClassifiedError](err); ok && (classified.Class != loom.ErrorClassPermanent || classified.Attempts != 1) {
			t.Fatalf("moderation must be terminal without retries: %v", err)
		}
	}
}

func policyRequest() loom.ChatRequest {
	return loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "fixture task"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}}
}

func consumePolicyModel(t *testing.T, model loom.ChatModel, streaming bool) (string, error) {
	t.Helper()
	if !streaming {
		response, err := model.Chat(t.Context(), policyRequest())
		if err != nil {
			return "", err
		}
		return response.Content, nil
	}
	stream, err := model.Stream(t.Context(), policyRequest())
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	var content strings.Builder
	for {
		chunk, err := stream.Recv()
		if err != nil {
			return content.String(), err
		}
		content.WriteString(chunk.ContentDelta)
	}
}

func policyModel(t *testing.T, provider string, status int, body string, sse, partial bool) loom.ChatModel {
	t.Helper()
	var strict map[string]any
	if err := jsonv2.Unmarshal([]byte(body), &strict); err != nil {
		t.Fatalf("invalid strict fixture: %v", err)
	}
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("fake and real chat-completions request contracts differ")
		}
		w.Header().Set("X-Request-ID", "trace-1")
		w.Header().Set("X-Client-Request-Id", "trace-1")
		w.Header().Set("X-Generation-ID", "generation-1")
		responseBody := body
		if sse {
			if provider == "openrouter" && strict["error"] != nil {
				responseBody = strings.Replace(body, "{", `{"id":"generation-1",`, 1)
			}
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		if sse {
			if partial {
				// Two visible chunks leave the retry probe before the rejection.
				_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial \"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n")
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", responseBody)
		} else {
			_, _ = fmt.Fprint(w, responseBody)
		}
	}))
	// A finite retry budget proves permanent errors do not use recovery attempts.
	retry := &loom.RetryConfig{Mode: loom.RetryModeFinite, MaxRetries: 1}
	var model loom.ChatModel
	var err error
	switch provider {
	case "deepseek":
		model, err = deepseek.New(deepseek.Config{APIKey: "fixture", ModelName: "fixture-model", BaseURL: server.URL, HTTPClient: server.Client(), Retry: retry})
	case "zhipuai":
		model, err = zhipuai.New(zhipuai.Config{APIKey: "fixture", ModelName: "fixture-model", BaseURL: server.URL, HTTPClient: server.Client(), Retry: retry})
	case "ark":
		model, err = ark.New(ark.Config{APIKey: "fixture", ModelName: "fixture-model", BaseURL: server.URL, HTTPClient: server.Client(), Retry: retry})
	case "openrouter":
		model, err = openrouter.New(openrouter.Config{APIKey: "fixture", ModelName: "fixture-model", BaseURL: server.URL, HTTPClient: server.Client(), Retry: retry})
	default:
		t.Fatalf("unknown fixture provider: %s", provider)
	}
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func Test_user_sees_captured_Ark_moderation_as_failed_delivery(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("user stream=%t", stream), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given a captured HTTP-200 refusal with text and a zero-token usage record.
				kind := "http"
				if stream {
					kind = "sse"
				}
				body, err := os.ReadFile("testdata/policy/ark-captured-" + kind + ".txt")
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
					}
					w.Header().Set("Content-Type", contentType)
					_, _ = w.Write(body)
				}))
				model, err := ark.New(ark.Config{APIKey: "fixture", ModelName: "fixture-model", BaseURL: server.URL, HTTPClient: server.Client(), Retry: &loom.RetryConfig{Mode: loom.RetryModeDisabled}})
				if err != nil {
					t.Fatal(err)
				}
				// When a real Loom turn delivers this captured provider response.
				turn, err := loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
					if stream {
						_, err := loom.StreamLLMToFinalAnswer(ctx, w, "final answer", model, policyRequest())
						return err
					}
					response, err := model.Chat(ctx, policyRequest())
					if err != nil {
						return err
					}
					if response.FinishReason == loom.FinishReasonContentFilter {
						return loom.ErrContentFilter
					}
					return w.FinalAnswer(ctx, response.Content)
				}, loom.RunOptions{ConversationID: "fixture-conversation", Input: loom.UserMessage{Text: "fixture task"}})
				// Then a natural-language refusal cannot be committed as a successful final answer.
				if !errors.Is(err, loom.ErrContentFilter) || turn.Status != loom.TurnStatusFailed || turn.CloseReason.Code != loom.CloseCodeContentFilter {
					t.Fatalf("captured moderation was lost: turn=%+v err=%v", turn, err)
				}
			})
		})
	}
}
