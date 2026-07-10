// Package modelfactory constructs Loom chat models from provider-neutral
// configuration. It does not prescribe where configuration comes from: an
// application may load it from a file, environment variables, a secrets
// manager, or a database.
package modelfactory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/ark"
	"github.com/loomagent/loom/providers/deepseek"
	"github.com/loomagent/loom/providers/openrouter"
)

// ErrInvalidConfig reports configuration that cannot construct a model.
var ErrInvalidConfig = errors.New("modelfactory: invalid config")

// Config contains the provider-neutral settings needed to construct a model.
// Secrets are consumed in memory and are never logged by this package.
type Config struct {
	// Provider is required and must be one of ProviderValues.
	Provider Provider
	// APIKey is required by every built-in provider.
	APIKey string
	// Model is the provider's model name or endpoint ID. DeepSeek permits an
	// empty value and then uses its provider default; other providers require it.
	Model string
	// BaseURL overrides the provider's default endpoint when non-empty.
	BaseURL string
	// Retry overrides Loom's default retry policy when non-nil.
	Retry *loom.RetryConfig
	// Capabilities declares the selected model's capabilities. A nil value
	// leaves capabilities unspecified and skips capability gating.
	Capabilities *loom.ModelCapabilities
}

// Build constructs a ChatModel from cfg.
//
// Provider selection is explicit. Build never infers a provider from BaseURL,
// so custom gateways and private endpoints behave consistently.
func Build(cfg Config) (loom.ChatModel, error) {
	if !cfg.Provider.Valid() {
		return nil, fmt.Errorf("%w: %w: %q", ErrInvalidConfig, ErrInvalidProvider, cfg.Provider)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("%w: APIKey is required", ErrInvalidConfig)
	}
	if cfg.Model != "" && strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("%w: Model cannot contain only whitespace", ErrInvalidConfig)
	}
	if cfg.BaseURL != "" && strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("%w: BaseURL cannot contain only whitespace", ErrInvalidConfig)
	}

	var (
		model loom.ChatModel
		err   error
	)
	switch cfg.Provider {
	case ProviderArk:
		model, err = ark.New(ark.Config{
			APIKey:       cfg.APIKey,
			ModelName:    cfg.Model,
			BaseURL:      cfg.BaseURL,
			Retry:        cfg.Retry,
			Capabilities: cfg.Capabilities,
		})
	case ProviderDeepSeek:
		model, err = deepseek.New(deepseek.Config{
			APIKey:       cfg.APIKey,
			ModelName:    cfg.Model,
			BaseURL:      cfg.BaseURL,
			Retry:        cfg.Retry,
			Capabilities: cfg.Capabilities,
		})
	case ProviderOpenRouter:
		model, err = openrouter.New(openrouter.Config{
			APIKey:       cfg.APIKey,
			ModelName:    cfg.Model,
			BaseURL:      cfg.BaseURL,
			Retry:        cfg.Retry,
			Capabilities: cfg.Capabilities,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("%w: build %s model: %w", ErrInvalidConfig, cfg.Provider, err)
	}
	return model, nil
}

// ConfigLoader loads model configuration by an application-defined ID.
// Implementations may use files, environment variables, secret managers, or
// databases; modelfactory itself has no storage dependency.
type ConfigLoader interface {
	LoadModelConfig(ctx context.Context, modelID string) (Config, error)
}

// Factory combines a ConfigLoader with Build for applications that select
// models by ID at runtime.
type Factory struct {
	Loader ConfigLoader
}

// Build loads modelID through f.Loader and constructs the selected model.
func (f Factory) Build(ctx context.Context, modelID string) (loom.ChatModel, error) {
	if f.Loader == nil {
		return nil, fmt.Errorf("%w: Loader is required", ErrInvalidConfig)
	}
	if strings.TrimSpace(modelID) == "" {
		return nil, fmt.Errorf("%w: modelID is required", ErrInvalidConfig)
	}
	cfg, err := f.Loader.LoadModelConfig(ctx, modelID)
	if err != nil {
		return nil, fmt.Errorf("modelfactory: load model %q: %w", modelID, err)
	}
	model, err := Build(cfg)
	if err != nil {
		return nil, fmt.Errorf("modelfactory: model %q: %w", modelID, err)
	}
	return model, nil
}
