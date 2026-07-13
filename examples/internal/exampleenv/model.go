// Package exampleenv contains environment loading shared by runnable examples.
package exampleenv

import (
	"errors"
	"os"
	"strings"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/modelfactory"
)

// OpenRouterModel builds the model selected by OPENROUTER_MODEL.
func OpenRouterModel() (loom.ChatModel, error) {
	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	model := strings.TrimSpace(os.Getenv("OPENROUTER_MODEL"))
	if apiKey == "" || model == "" {
		return nil, errors.New("set OPENROUTER_API_KEY and OPENROUTER_MODEL")
	}
	return modelfactory.Build(modelfactory.Config{
		Provider: modelfactory.ProviderOpenRouter,
		APIKey:   apiKey,
		Model:    model,
	})
}
