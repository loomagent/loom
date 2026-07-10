package modelfactory

import (
	"context"
	"errors"
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
