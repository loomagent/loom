package modelfactory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loomagent/loom"
)

func TestBuild(t *testing.T) {
	caps := &loom.ModelCapabilities{MaxContextTokens: 128_000}
	tests := []struct {
		name     string
		cfg      Config
		wantName string
	}{
		{
			name:     "zhipuai",
			cfg:      Config{Provider: ProviderZhipuAI, APIKey: "key", Model: "glm-5.3", Capabilities: caps},
			wantName: "zhipuai/glm-5.3",
		},
		{
			name: "ark",
			cfg: Config{
				Provider:     ProviderArk,
				APIKey:       "test-key",
				Model:        "ep-test",
				Capabilities: caps,
			},
			wantName: "ark/ep-test",
		},
		{
			name: "deepseek default model",
			cfg: Config{
				Provider:     ProviderDeepSeek,
				APIKey:       "test-key",
				Capabilities: caps,
			},
			wantName: "deepseek/deepseek-v4-flash",
		},
		{
			name: "openrouter",
			cfg: Config{
				Provider:     ProviderOpenRouter,
				APIKey:       "test-key",
				Model:        "x-ai/grok-4.3",
				Capabilities: caps,
			},
			wantName: "openrouter/x-ai/grok-4.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, err := Build(tt.cfg)
			if err != nil {
				t.Fatalf("Build(): %v", err)
			}
			if got := model.Name(); got != tt.wantName {
				t.Fatalf("Name() = %q, want %q", got, tt.wantName)
			}
			if got := model.Capabilities().MaxContextTokens; got != caps.MaxContextTokens {
				t.Fatalf("MaxContextTokens = %d, want %d", got, caps.MaxContextTokens)
			}
		})
	}
}

func TestBuildRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{name: "zero provider", cfg: Config{APIKey: "key"}, wantErr: ErrInvalidProvider},
		{name: "unknown provider", cfg: Config{Provider: "other", APIKey: "key"}, wantErr: ErrInvalidProvider},
		{name: "missing API key", cfg: Config{Provider: ProviderDeepSeek}, wantErr: ErrInvalidConfig},
		{name: "zhipuai missing model", cfg: Config{Provider: ProviderZhipuAI, APIKey: "key"}, wantErr: ErrInvalidConfig},
		{name: "ark missing model", cfg: Config{Provider: ProviderArk, APIKey: "key"}, wantErr: ErrInvalidConfig},
		{name: "openrouter missing model", cfg: Config{Provider: ProviderOpenRouter, APIKey: "key"}, wantErr: ErrInvalidConfig},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, err := Build(tt.cfg)
			if model != nil {
				t.Fatalf("Build() model = %T, want nil", model)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Build() error = %v, want errors.Is(_, %v)", err, tt.wantErr)
			}
		})
	}
}

type stubLoader struct {
	cfg   Config
	err   error
	gotID string
}

func (l *stubLoader) LoadModelConfig(_ context.Context, modelID string) (Config, error) {
	l.gotID = modelID
	return l.cfg, l.err
}

func TestFactoryBuild(t *testing.T) {
	loader := &stubLoader{cfg: Config{
		Provider: ProviderOpenRouter,
		APIKey:   "test-key",
		Model:    "openai/gpt-test",
	}}
	factory := Factory{Loader: loader}

	model, err := factory.Build(context.Background(), "primary")
	if err != nil {
		t.Fatal(err)
	}
	if loader.gotID != "primary" {
		t.Fatalf("loader modelID = %q", loader.gotID)
	}
	if got := model.Name(); got != "openrouter/openai/gpt-test" {
		t.Fatalf("Name() = %q", got)
	}
}

func TestFactoryBuildErrors(t *testing.T) {
	t.Run("nil loader", func(t *testing.T) {
		_, err := (Factory{}).Build(context.Background(), "model")
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("empty model ID", func(t *testing.T) {
		_, err := (Factory{Loader: &stubLoader{}}).Build(context.Background(), " ")
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("loader error", func(t *testing.T) {
		want := errors.New("load failed")
		_, err := (Factory{Loader: &stubLoader{err: want}}).Build(context.Background(), "model")
		if !errors.Is(err, want) {
			t.Fatalf("error = %v", err)
		}
	})
}

// recordingLimiter records what it was asked to admit, which is how a host sees the seam.
type recordingLimiter struct {
	acquired []loom.AttemptMeta
	finished []loom.AttemptResult
}

func (l *recordingLimiter) Acquire(_ context.Context, meta loom.AttemptMeta) (loom.AttemptPermit, error) {
	l.acquired = append(l.acquired, meta)
	return recordingPermit{l}, nil
}

type recordingPermit struct{ limiter *recordingLimiter }

func (p recordingPermit) Finish(result loom.AttemptResult) {
	p.limiter.finished = append(p.limiter.finished, result)
}

// A limiter a caller declares reaches the provider: it is asked before each physical attempt,
// with the quota identity that was declared, and told how the attempt ended. Loom ships no
// limiter, so this is the whole of the framework's part in pacing.
func TestBuildPassesAnAttemptLimiterThrough(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"model":"deepseek-chat","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	limiter := &recordingLimiter{}
	model, err := Build(Config{
		Provider:       ProviderDeepSeek,
		APIKey:         "key",
		Model:          "deepseek-chat",
		BaseURL:        server.URL,
		HTTPClient:     server.Client(),
		AttemptLimiter: limiter,
		QuotaKey:       "credential-fingerprint",
		QuotaLabel:     "deepseek",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.Chat(context.Background(), loom.ChatRequest{
		Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
		Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
	}); err != nil {
		t.Fatal(err)
	}
	if len(limiter.acquired) != 1 {
		t.Fatalf("acquired = %+v", limiter.acquired)
	}
	meta := limiter.acquired[0]
	if meta.QuotaKey != "credential-fingerprint" || meta.QuotaLabel != "deepseek" || meta.Model != "deepseek-chat" {
		t.Fatalf("meta = %+v", meta)
	}
	if len(limiter.finished) != 1 || !limiter.finished[0].Success {
		t.Fatalf("finished = %+v", limiter.finished)
	}
}

// A limiter with no quota would pace every attempt under a key nobody else shares, which looks
// like pacing and is not, so it is refused where it is configured.
func TestBuildRequiresAQuotaWithALimiter(t *testing.T) {
	_, err := Build(Config{
		Provider:       ProviderDeepSeek,
		APIKey:         "key",
		Model:          "deepseek-chat",
		AttemptLimiter: &recordingLimiter{},
	})
	if err == nil || !strings.Contains(err.Error(), "QuotaKey") {
		t.Fatalf("error = %v", err)
	}
}

// A caller's own retry policy survives the limiter: the limiter is added to it rather than
// replacing it, which the attempt count shows.
func TestBuildKeepsTheCallersRetryPolicy(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":{"message":"down"}}`)
	}))
	limiter := &recordingLimiter{}
	model, err := Build(Config{
		Provider:       ProviderDeepSeek,
		APIKey:         "key",
		Model:          "deepseek-chat",
		BaseURL:        server.URL,
		HTTPClient:     server.Client(),
		Retry:          &loom.RetryConfig{Mode: loom.RetryModeDisabled},
		AttemptLimiter: limiter,
		QuotaKey:       "fingerprint",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.Chat(context.Background(), loom.ChatRequest{
		Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
		Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
	}); err == nil {
		t.Fatal("an endpoint that is down must fail")
	}
	// Retries are disabled by the caller's policy, so exactly one attempt was admitted.
	if len(limiter.acquired) != 1 {
		t.Fatalf("acquired = %d attempts, want the caller's policy to stand", len(limiter.acquired))
	}
	if len(limiter.finished) != 1 || limiter.finished[0].Success {
		t.Fatalf("finished = %+v", limiter.finished)
	}
}
