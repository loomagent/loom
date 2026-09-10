package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/openai/openai-go/v3/packages/ssestream"
	goseek "github.com/storynap/goseek"
)

// Keep protocol serialization independent of the older SDK's capability enum.
// In particular, its ResponseFormat only has a type field and cannot carry a
// JSON Schema. Model support is declared by the caller or observed by probes.
type chatRequest struct {
	goseek.ChatCompletionRequest
	ResponseFormat any `json:"response_format,omitempty"`
}

type chatUsage struct {
	goseek.Usage
	CompletionTokensDetails *struct {
		ReasoningTokens *uint64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type chatResponse struct {
	Choices []goseek.ChatCompletionChoice `json:"choices"`
	Model   string                        `json:"model"`
	Usage   *chatUsage                    `json:"usage"`
}

type chatChunk struct {
	Choices []goseek.ChatCompletionChunkChoice `json:"choices"`
	Model   string                             `json:"model"`
	Usage   *chatUsage                         `json:"usage"`
}

type chatClient struct {
	endpoint string
	apiKey   string
}

func newChatClient(apiKey, baseURL string) (*chatClient, error) {
	if baseURL == "" {
		baseURL = goseek.DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid DeepSeek base URL %q", baseURL)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	return &chatClient{endpoint: u.String(), apiKey: apiKey}, nil
}

// Send the requested protocol fields as-is. Neither model names nor SDK enums
// decide whether the upstream model can accept them. Loom owns all retries.
func (c *chatClient) send(ctx context.Context, body chatRequest) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal DeepSeek request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if body.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var failure struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &failure)
	message := failure.Error.Message
	if message == "" {
		message = failure.Message
	}
	if message == "" {
		message = failure.Detail
	}
	return nil, &goseek.APIError{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: raw, Message: message}
}

func (c *chatClient) CreateChatCompletion(ctx context.Context, req chatRequest) (*chatResponse, error) {
	resp, err := c.send(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *chatClient) CreateChatCompletionStream(ctx context.Context, req chatRequest) (*ssestream.Stream[chatChunk], error) {
	req.Stream = true
	resp, err := c.send(ctx, req)
	if err != nil {
		return nil, err
	}
	return ssestream.NewStream[chatChunk](ssestream.NewDecoder(resp), nil), nil
}
