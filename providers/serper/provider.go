// Package serper implements web search through the Serper Google Search API.
package serper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loomagent/loom/tools/web"
)

const defaultEndpoint = "https://google.serper.dev/search"

// HTTPError describes a non-successful response from Serper.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e HTTPError) Error() string {
	return fmt.Sprintf("serper: HTTP %d: %s", e.StatusCode, truncate(e.Body, 500))
}

// Option configures Client.
type Option func(*Client)

// WithHTTPClient replaces the default HTTP client.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

// WithEndpoint overrides the API endpoint, primarily for compatible services
// and tests.
func WithEndpoint(endpoint string) Option {
	return func(c *Client) {
		if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
			c.endpoint = endpoint
		}
	}
}

// Client is safe for concurrent use when its HTTP client is safe for
// concurrent use.
type Client struct {
	apiKey   string
	http     *http.Client
	endpoint string
}

// New creates a Serper search provider.
func New(apiKey string, options ...Option) *Client {
	c := &Client{
		apiKey:   strings.TrimSpace(apiKey),
		http:     &http.Client{Timeout: 30 * time.Second},
		endpoint: defaultEndpoint,
	}
	for _, option := range options {
		if option != nil {
			option(c)
		}
	}
	return c
}

type searchRequest struct {
	Query      string `json:"q"`
	NumResults int    `json:"num,omitempty"`
}

type searchResponse struct {
	Organic []result `json:"organic"`
	Message string   `json:"message,omitempty"`
}

type result struct {
	Title    string `json:"title"`
	Link     string `json:"link"`
	Snippet  string `json:"snippet"`
	Date     string `json:"date,omitempty"`
	Position int    `json:"position,omitempty"`
}

// Search implements web.Searcher.
func (c *Client) Search(ctx context.Context, request web.SearchRequest) (web.SearchResponse, error) {
	query := strings.TrimSpace(request.Query)
	if query == "" {
		return web.SearchResponse{}, errors.New("serper: query is required")
	}
	payload, err := json.Marshal(searchRequest{Query: query, NumResults: request.Limit})
	if err != nil {
		return web.SearchResponse{}, fmt.Errorf("serper: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return web.SearchResponse{}, fmt.Errorf("serper: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return web.SearchResponse{}, fmt.Errorf("serper: request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return web.SearchResponse{}, fmt.Errorf("serper: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return web.SearchResponse{}, HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	var parsed searchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return web.SearchResponse{}, fmt.Errorf("serper: decode response: %w", err)
	}
	if message := strings.TrimSpace(parsed.Message); message != "" && len(parsed.Organic) == 0 {
		return web.SearchResponse{}, fmt.Errorf("serper: API error: %s", message)
	}
	results := make([]web.SearchResult, 0, len(parsed.Organic))
	for _, item := range parsed.Organic {
		metadata := map[string]string{}
		if item.Date != "" {
			metadata["date"] = item.Date
			metadata["date_source"] = "serper.organic.date"
		}
		if item.Position > 0 {
			metadata["position"] = fmt.Sprint(item.Position)
		}
		if len(metadata) == 0 {
			metadata = nil
		}
		results = append(results, web.SearchResult{Title: item.Title, URL: item.Link, Snippet: item.Snippet, Metadata: metadata})
	}
	return web.SearchResponse{Results: results, Metadata: map[string]string{"provider": "serper"}}, nil
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
