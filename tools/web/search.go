package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/loomagent/loom"
)

// Searcher performs provider-specific web searches behind a stable interface.
type Searcher interface {
	Search(ctx context.Context, request SearchRequest) (SearchResponse, error)
}

// SearchRequest describes a search independent of any provider SDK.
type SearchRequest struct {
	Query string
	Limit int
}

// SearchResponse contains normalized search results.
type SearchResponse struct {
	Results  []SearchResult    `json:"results"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// SearchResult is one normalized result.
type SearchResult struct {
	Title       string            `json:"title,omitempty"`
	URL         string            `json:"url"`
	Snippet     string            `json:"snippet,omitempty"`
	PublishedAt *time.Time        `json:"publishedAt,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// SearchToolOptions configures NewSearchTool.
type SearchToolOptions struct {
	Name         string
	Description  string
	DefaultLimit int
	MaxLimit     int
}

// NewSearchTool exposes Searcher as a Loom tool.
func NewSearchTool(searcher Searcher, options SearchToolOptions) (loom.Tool, error) {
	if searcher == nil {
		return nil, errors.New("web: Searcher is required")
	}
	name := strings.TrimSpace(options.Name)
	if name == "" {
		name = "web_search"
	}
	defaultLimit := options.DefaultLimit
	if defaultLimit <= 0 {
		defaultLimit = 10
	}
	maxLimit := options.MaxLimit
	if maxLimit <= 0 {
		maxLimit = 20
	}
	if defaultLimit > maxLimit {
		return nil, fmt.Errorf("web: DefaultLimit %d exceeds MaxLimit %d", defaultLimit, maxLimit)
	}
	description := strings.TrimSpace(options.Description)
	if description == "" {
		description = "Search the web for current information and return normalized results with titles, URLs, snippets, and publication dates when available."
	}
	params := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"query": {Type: "string", Description: "Search query."},
			"limit": {Type: "integer", Description: fmt.Sprintf("Maximum results to return (default %d, max %d).", defaultLimit, maxLimit)},
		},
		Required: []string{"query"},
	}
	return loom.NewTool(name, description, params, func(ctx context.Context, arguments string) (string, error) {
		var input struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(arguments), &input); err != nil {
			return "", fmt.Errorf("web search: parse arguments: %w", err)
		}
		input.Query = strings.TrimSpace(input.Query)
		if input.Query == "" {
			return "", errors.New("web search: query is required")
		}
		if input.Limit == 0 {
			input.Limit = defaultLimit
		}
		if input.Limit < 1 || input.Limit > maxLimit {
			return "", fmt.Errorf("web search: limit must be between 1 and %d", maxLimit)
		}
		response, err := searcher.Search(ctx, SearchRequest{Query: input.Query, Limit: input.Limit})
		if err != nil {
			return "", fmt.Errorf("web search: %w", err)
		}
		data, err := json.Marshal(response)
		if err != nil {
			return "", fmt.Errorf("web search: marshal response: %w", err)
		}
		return string(data), nil
	}), nil
}
