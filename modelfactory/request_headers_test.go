package modelfactory_test

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/cryptotest"
	"testing/synctest"
	"time"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/modelfactory"
	"github.com/loomagent/loom/modelprobe"
	"github.com/loomagent/loom/providers/ark"
	"github.com/loomagent/loom/providers/deepseek"
	"github.com/loomagent/loom/providers/openrouter"
	"github.com/loomagent/loom/providers/zhipuai"
)

type accountConfigs map[string]modelfactory.Config

func (c accountConfigs) LoadModelConfig(_ context.Context, id string) (modelfactory.Config, error) {
	cfg, ok := c[id]
	if !ok {
		return modelfactory.Config{}, errors.New("unknown configured account")
	}
	return cfg, nil
}

func constructHeaderModel(cfg modelfactory.Config, entry string) (loom.ChatModel, error) {
	switch entry {
	case "Build":
		return modelfactory.Build(cfg)
	case "Factory":
		return (modelfactory.Factory{Loader: accountConfigs{"account": cfg}}).Build(context.Background(), "account")
	}
	switch cfg.Provider {
	case modelfactory.ProviderArk:
		return ark.New(ark.Config{APIKey: cfg.APIKey, ModelName: cfg.Model, BaseURL: cfg.BaseURL, HTTPClient: cfg.HTTPClient, Retry: cfg.Retry, Capabilities: cfg.Capabilities, RequestHeaders: cfg.RequestHeaders})
	case modelfactory.ProviderDeepSeek:
		return deepseek.New(deepseek.Config{APIKey: cfg.APIKey, ModelName: cfg.Model, BaseURL: cfg.BaseURL, HTTPClient: cfg.HTTPClient, Retry: cfg.Retry, Capabilities: cfg.Capabilities, RequestHeaders: cfg.RequestHeaders})
	case modelfactory.ProviderOpenRouter:
		return openrouter.New(openrouter.Config{APIKey: cfg.APIKey, ModelName: cfg.Model, BaseURL: cfg.BaseURL, HTTPClient: cfg.HTTPClient, Retry: cfg.Retry, Capabilities: cfg.Capabilities, RequestHeaders: cfg.RequestHeaders})
	case modelfactory.ProviderZhipuAI:
		return zhipuai.New(zhipuai.Config{APIKey: cfg.APIKey, ModelName: cfg.Model, BaseURL: cfg.BaseURL, HTTPClient: cfg.HTTPClient, Retry: cfg.Retry, Capabilities: cfg.Capabilities, RequestHeaders: cfg.RequestHeaders})
	default:
		return nil, modelfactory.ErrInvalidProvider
	}
}

func headerAnswer(t *testing.T, model loom.ChatModel, streaming bool) string {
	t.Helper()
	req := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hello"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}}
	if !streaming {
		response, err := model.Chat(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		return response.Content
	}
	stream, err := model.Stream(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var answer string
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return answer
		}
		if err != nil {
			t.Fatal(err)
		}
		answer += chunk.ContentDelta
	}
}

func headerFixture(t *testing.T, raw string) string {
	t.Helper()
	var value any
	if err := jsonv2.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	return raw
}

func respondToHeaderRequest(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var body struct {
		Stream bool `json:"stream"`
	}
	if err := jsonv2.UnmarshalRead(r.Body, &body); err != nil {
		t.Errorf("invalid request JSON: %v", err)
	}
	if !body.Stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, headerFixture(t, `{"id":"fake","model":"fake-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", headerFixture(t, `{"id":"fake","model":"fake-model","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`))
}

func Test_user_extra_headers_reach_every_provider_constructor_and_retry(t *testing.T) {
	// Given custom account headers, When a user calls HTTP or SSE through any
	// constructor and temporary failures force retries, Then all requests carry
	// the original headers and the successful answer is delivered.
	for _, provider := range modelfactory.ProviderValues() {
		for _, entry := range []string{"direct", "Build", "Factory"} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("user %s %s stream=%t", provider, entry, streaming), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						var calls atomic.Int32
						server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if got := r.Header.Values("X-Account-Policy"); len(got) != 1 || got[0] != "configured-value" {
								t.Errorf("headers on wire=%v", got)
							}
							if r.Header.Get("X-Extra") != "second-value" {
								t.Error("second custom header was dropped")
							}
							if r.Header.Get("Authorization") != "Bearer fake-key" {
								t.Error("credential was not preserved")
							}
							if calls.Add(1) <= 3 {
								w.Header().Set("Content-Type", "application/json")
								w.WriteHeader(http.StatusServiceUnavailable)
								_, _ = fmt.Fprint(w, headerFixture(t, `{"error":{"code":"ServiceUnavailable","message":"try again"}}`))
								return
							}
							respondToHeaderRequest(t, w, r)
						}))
						headers := map[string]string{"X-Account-Policy": "configured-value", "x-account-policy": "configured-value", "X-Extra": "second-value"}
						model, err := constructHeaderModel(modelfactory.Config{Provider: provider, APIKey: "fake-key", Model: "fake-model", BaseURL: server.URL, HTTPClient: server.Client(), Retry: &loom.RetryConfig{MaxRetries: 4, InitialBackoff: time.Millisecond}, RequestHeaders: headers}, entry)
						if err != nil {
							t.Fatal(err)
						}
						headers["X-Account-Policy"] = "mutated-after-construction"
						delete(headers, "X-Extra")
						if got := headerAnswer(t, model, streaming); got != "ok" {
							t.Fatalf("answer=%q", got)
						}
						if calls.Load() < 4 {
							t.Fatal("temporary failures did not exercise retry")
						}
					})
				})
			}
		}
	}
}

func Test_user_header_configuration_is_isolated_between_models_and_shared_clients(t *testing.T) {
	// Given concurrent models sharing an HTTP client with different or absent
	// account settings, When HTTP/SSE calls run, Then headers never cross accounts,
	// providers, unconfigured models or the caller's original HTTP client.
	synctest.Test(t, func(t *testing.T) {
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			want := ""
			if strings.HasSuffix(key, "-configured") {
				want = key
			}
			if got := r.Header.Get("X-Account"); got != want {
				t.Errorf("account %s got header=%q, want=%q", key, got, want)
			}
			respondToHeaderRequest(t, w, r)
		}))
		var wg sync.WaitGroup
		for _, provider := range modelfactory.ProviderValues() {
			for _, configured := range []bool{false, true} {
				cfg := modelfactory.Config{Provider: provider, APIKey: string(provider) + "-plain", Model: "fake-model", BaseURL: server.URL, HTTPClient: server.Client(), Retry: &loom.RetryConfig{Mode: loom.RetryModeDisabled}}
				if configured {
					cfg.APIKey = string(provider) + "-configured"
					cfg.RequestHeaders = map[string]string{"X-Account": cfg.APIKey}
				}
				model, err := modelfactory.Build(cfg)
				if err != nil {
					t.Fatal(err)
				}
				wg.Go(func() {
					for _, streaming := range []bool{false, true} {
						if got := headerAnswer(t, model, streaming); got != "ok" {
							t.Errorf("answer=%q", got)
						}
					}
				})
			}
		}
		wg.Wait()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	})
}

func Test_user_invalid_extra_headers_fail_before_any_request(t *testing.T) {
	// Given malformed, ambiguous or protocol-owned headers, When a user creates
	// a model, Then every provider rejects them locally with a common typed error
	// and never includes header values in that error.
	invalid := []map[string]string{
		{"": "private-value"}, {" X-Policy": "private-value"}, {"X:Policy": "private-value"},
		{"X-Policy": "private-value\r\nInjected: yes"}, {"X-Policy": "private-value\x00"},
		{"X-Policy": "private-value", "x-policy": "different-private-value"},
		{"Authorization": "private-value"}, {"Proxy-Authorization": "private-value"},
		{"Host": "private-value"}, {"Content-Type": "private-value"}, {"Accept": "private-value"},
		{"Content-Length": "private-value"}, {"Transfer-Encoding": "private-value"},
		{"Connection": "private-value"}, {"Trailer": "private-value"},
	}
	for _, provider := range modelfactory.ProviderValues() {
		for _, entry := range []string{"direct", "Build", "Factory"} {
			for _, headers := range invalid {
				_, err := constructHeaderModel(modelfactory.Config{Provider: provider, APIKey: "fake-key", Model: "fake-model", RequestHeaders: headers}, entry)
				if !errors.Is(err, loom.ErrInvalidRequestHeaders) {
					t.Fatalf("provider=%s entry=%s err=%v", provider, entry, err)
				}
				if entry != "direct" && !errors.Is(err, modelfactory.ErrInvalidConfig) {
					t.Fatalf("missing factory configuration error: %v", err)
				}
				if strings.Contains(err.Error(), "private-value") {
					t.Fatal("error exposed a custom header value")
				}
			}
		}
	}
}

func Test_user_extra_headers_survive_SSE_first_frame_retry(t *testing.T) {
	// Given HTTP 200 followed by a temporary error in the first SSE frame, When
	// the user retries before receiving content, Then the new stream carries the
	// same account header and delivers the complete answer.
	for _, provider := range modelfactory.ProviderValues() {
		t.Run("user "+string(provider), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("X-Account") != "configured-value" {
						t.Error("header missing from stream attempt")
					}
					if requests.Add(1) == 1 {
						w.Header().Set("Content-Type", "text/event-stream")
						frame := `{"error":{"code":"ServiceUnavailable","message":"try again"}}`
						if provider == modelfactory.ProviderOpenRouter {
							frame = `{"error":{"code":503,"message":"try again"}}`
						}
						if provider == modelfactory.ProviderZhipuAI {
							frame = `{"error":{"code":"1305","message":"try again"}}`
						}
						_, _ = fmt.Fprintf(w, "data: %s\n\n", headerFixture(t, frame))
						return
					}
					respondToHeaderRequest(t, w, r)
				}))
				model, err := modelfactory.Build(modelfactory.Config{Provider: provider, APIKey: "fake-key", Model: "fake-model", BaseURL: server.URL, HTTPClient: server.Client(), Retry: &loom.RetryConfig{MaxRetries: 1, InitialBackoff: time.Millisecond}, RequestHeaders: map[string]string{"X-Account": "configured-value"}})
				if err != nil {
					t.Fatal(err)
				}
				if answer := headerAnswer(t, model, true); answer != "ok" {
					t.Fatalf("answer=%q", answer)
				}
			})
		})
	}
}

func Test_user_capability_probes_keep_account_request_headers(t *testing.T) {
	// Given a shared model configuration, When the user runs the real capability
	// auditor (which rebuilds models with synthetic capabilities), Then all probe
	// requests retain the account header and produce usable capability evidence.
	cryptotest.SetGlobalRandom(t, 42)
	for _, provider := range modelfactory.ProviderValues() {
		t.Run("user "+string(provider), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("X-Probe-Account") != "configured-value" {
						t.Error("probe bypassed the account header configuration")
					}
					var request struct {
						Thinking struct {
							Type string `json:"type"`
						} `json:"thinking"`
						Reasoning struct {
							Enabled bool `json:"enabled"`
						} `json:"reasoning"`
						ResponseFormat struct {
							Type       string `json:"type"`
							JSONSchema struct {
								Schema loom.Schema `json:"schema"`
							} `json:"json_schema"`
						} `json:"response_format"`
					}
					if err := jsonv2.UnmarshalRead(r.Body, &request); err != nil {
						t.Errorf("invalid probe request: %v", err)
					}
					content := "ok"
					if request.ResponseFormat.Type != "" {
						value := map[string]any{"ok": true}
						if nonce := request.ResponseFormat.JSONSchema.Schema.Properties["nonce"]; nonce != nil {
							value["nonce"] = nonce.Const
						}
						encoded, err := jsonv2.Marshal(value)
						if err != nil {
							t.Errorf("fake structured response: %v", err)
						}
						content = string(encoded)
					}
					tokens := 0
					if request.Thinking.Type == "enabled" || request.Reasoning.Enabled {
						tokens = 1
					}
					w.Header().Set("Content-Type", "application/json")
					_ = jsonv2.MarshalWrite(w, map[string]any{
						"model": "fake-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
						"usage": map[string]any{"completion_tokens_details": map[string]any{"reasoning_tokens": tokens}},
					})
				}))
				base := modelfactory.Config{Provider: provider, APIKey: "fake-key", Model: "fake-model", BaseURL: server.URL, HTTPClient: server.Client(), Retry: &loom.RetryConfig{Mode: loom.RetryModeDisabled}, RequestHeaders: map[string]string{"X-Probe-Account": "configured-value"}}
				report, err := modelprobe.Probe(t.Context(), modelprobe.BuilderFunc(func(_ context.Context, caps loom.ModelCapabilities) (loom.ChatModel, error) {
					cfg := base
					cfg.Capabilities = &caps
					return modelfactory.Build(cfg)
				}), modelprobe.Options{})
				if err != nil {
					t.Fatal(err)
				}
				if len(report.Checks) < 5 {
					t.Fatalf("incomplete probe report=%+v", report)
				}
				for _, check := range report.Checks {
					if check.Evidence.Acceptance != "accepted" {
						t.Fatalf("probe failed: %+v", check)
					}
				}
				if report.Observed.StructuredOutput != loom.StructuredOutputJSONSchema {
					t.Fatalf("structured probe evidence=%+v", report)
				}
			})
		})
	}
}
