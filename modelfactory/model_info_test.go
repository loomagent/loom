package modelfactory

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loomagent/loom"
)

func TestModelInfoSurvivesFactoryAndPhysicalRetries(t *testing.T) {
	for _, provider := range []Provider{ProviderArk, ProviderDeepSeek, ProviderOpenRouter, ProviderZhipuAI} {
		for _, streaming := range []bool{false, true} {
			t.Run(string(provider)+map[bool]string{false: "/chat", true: "/stream"}[streaming], func(t *testing.T) {
				var requests atomic.Int64
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					if strings.Contains(string(body), "instance-A") || strings.Contains(string(body), "snapshot-A") {
						t.Error("host identity leaked to provider")
					}
					var sent map[string]any
					if err := jsonv2.Unmarshal(body, &sent); err != nil {
						t.Error(err)
						return
					}
					if sent["model"] != "alias" {
						t.Errorf("request target=%v", sent["model"])
					}
					if requests.Add(1) == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = io.WriteString(w, `{"error":{"message":"retry fixture"}}`)
						return
					}
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"model\":\"provider-revision\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"model":"provider-revision","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
					}
				}))
				ref := loom.ModelRef{InstanceID: "instance-A", SnapshotID: "snapshot-A"}
				limiter := &recordingLimiter{}
				policy := &loom.RetryConfig{MaxRetries: 1, InitialBackoff: time.Nanosecond, MaxBackoff: time.Nanosecond, AttemptMeta: loom.AttemptMeta{ModelInfo: loom.ModelInfo{Ref: loom.ModelRef{InstanceID: "wrong-input"}}}}
				cfg := Config{Provider: provider, Ref: ref, APIKey: "fixture-key", Model: "alias", BaseURL: server.URL, HTTPClient: server.Client(), Retry: policy, AttemptLimiter: limiter, QuotaKey: "private-quota"}
				loader := &stubLoader{cfg: cfg}
				model, err := (Factory{Loader: loader}).Build(t.Context(), "host-config-key")
				if err != nil {
					t.Fatal(err)
				}
				want := loom.ModelInfo{Ref: ref, Provider: string(provider), RequestModel: "alias"}
				cfg.Ref.SnapshotID = "new-config"
				returned := loom.DescribeModel(model)
				returned.Ref.SnapshotID = "edited-return"
				if loom.DescribeModel(model) != want {
					t.Fatal("construction metadata was mutable")
				}
				req := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hello"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}}
				if streaming {
					stream, err := model.Stream(t.Context(), req)
					if err != nil {
						t.Fatal(err)
					}
					defer stream.Close()
					var text string
					for {
						chunk, err := stream.Recv()
						if chunk != nil {
							text += chunk.ContentDelta
						}
						if errors.Is(err, io.EOF) {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					if text != "ok" {
						t.Fatalf("stream content changed: %q", text)
					}
					if err := stream.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					resp, err := model.Chat(t.Context(), req)
					if err != nil {
						t.Fatal(err)
					}
					if resp.Content != "ok" || resp.Model != "provider-revision" {
						t.Fatalf("response changed: %+v", resp)
					}
				}
				if requests.Load() != 2 || len(limiter.acquired) != 2 || len(limiter.finished) != 2 {
					t.Fatalf("retry/admission semantics changed: requests=%d acquired=%d finished=%d", requests.Load(), len(limiter.acquired), len(limiter.finished))
				}
				for _, meta := range limiter.acquired {
					if meta.ModelInfo != want || meta.QuotaKey != "private-quota" {
						t.Fatalf("physical attempt attributed incorrectly: %+v", meta)
					}
				}
				if limiter.finished[0].Success || !limiter.finished[1].Success {
					t.Fatalf("attempt outcomes changed: %+v", limiter.finished)
				}
				if policy.AttemptMeta.ModelInfo.Ref.InstanceID != "wrong-input" {
					t.Fatal("construction mutated caller retry policy")
				}
				t.Logf("attempts=2 target=%+v response=provider-revision", want)
			})
		}
	}
}

func TestModelInfoUsesResolvedDefaultButNeverInfersHostRef(t *testing.T) {
	model, err := Build(Config{Provider: ProviderDeepSeek, APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	got := loom.DescribeModel(model)
	if got.Ref != (loom.ModelRef{}) || got.RequestModel != "deepseek-v4-flash" || got.Provider != "deepseek" {
		t.Fatalf("default target wrong: %+v", got)
	}
}

func TestModelInfoDisabledRetriesAdmitOnlyOnePhysicalRequest(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "stream"}[streaming], func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":{"message":"down"}}`)
			}))
			limiter := &recordingLimiter{}
			model, err := Build(Config{Provider: ProviderArk, Ref: loom.ModelRef{InstanceID: "account:alias", SnapshotID: "snapshot"}, APIKey: "fixture", Model: "alias", BaseURL: server.URL, HTTPClient: server.Client(), Retry: &loom.RetryConfig{Mode: loom.RetryModeDisabled}, AttemptLimiter: limiter, QuotaKey: "quota"})
			if err != nil {
				t.Fatal(err)
			}
			req := loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "question"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}}
			if streaming {
				_, err = model.Stream(t.Context(), req)
			} else {
				_, err = model.Chat(t.Context(), req)
			}
			if err == nil || requests.Load() != 1 || len(limiter.acquired) != 1 || len(limiter.finished) != 1 || limiter.finished[0].Success {
				t.Fatalf("disabled retry bypassed admission: err=%v requests=%d acquired=%d finished=%d", err, requests.Load(), len(limiter.acquired), len(limiter.finished))
			}
			if limiter.acquired[0].ModelInfo != loom.DescribeModel(model) {
				t.Fatal("failed request lost its model binding")
			}
		})
	}
}
