// Package unifuncs implements document reading through the Unifuncs Web
// Reader API.
package unifuncs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/loomagent/loom/tools/web"
	"github.com/loomagent/loom/tools/web/sourcedate"
)

const (
	defaultEndpoint   = "https://api.unifuncs.com/api/web-reader/read"
	defaultTimeout    = 60 * time.Second
	defaultInterval   = 300 * time.Millisecond
	defaultMaxRetries = 3
	defaultRetryDelay = time.Second
	defaultMaxDelay   = 30 * time.Second
	maxResponseBytes  = 8 << 20
)

// HTTPError describes a non-successful response from Unifuncs.
type HTTPError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e HTTPError) Error() string {
	return fmt.Sprintf("unifuncs: HTTP %d: %s", e.StatusCode, truncate(e.Body, 500))
}

// IsAccountFatal reports whether authentication or account credit prevents all
// reads from succeeding.
func IsAccountFatal(err error) bool {
	var httpErr HTTPError
	return errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusPaymentRequired)
}

// ReadStats describes the work performed by one logical read.
type ReadStats struct {
	Attempts   int
	Duration   time.Duration
	Timeout    time.Duration
	MaxRetries int
}

// Option configures Client.
type Option func(*Client)

func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

func WithEndpoint(endpoint string) Option {
	return func(c *Client) {
		if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
			c.endpoint = endpoint
		}
	}
}

func WithReadTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		if timeout > 0 {
			c.timeout = timeout
		}
	}
}

func WithMaxRetries(maxRetries int) Option {
	return func(c *Client) {
		if maxRetries >= 0 {
			c.maxRetries = maxRetries
		}
	}
}

func WithRetryDelays(initial, maximum time.Duration) Option {
	return func(c *Client) {
		if initial > 0 {
			c.retryDelay = initial
		}
		if maximum > 0 {
			c.maxRetryDelay = maximum
		}
	}
}

func WithRequestInterval(interval time.Duration) Option {
	return func(c *Client) {
		if interval >= 0 {
			c.limiter.interval = interval
		}
	}
}

// Client is safe for concurrent use.
type Client struct {
	apiKey        string
	http          *http.Client
	endpoint      string
	timeout       time.Duration
	maxRetries    int
	retryDelay    time.Duration
	maxRetryDelay time.Duration
	limiter       *limiter
}

var _ web.WebReader = (*Client)(nil)

// New creates an Unifuncs reader. An empty key falls back to
// UNIFUNCS_API_KEY.
func New(apiKey string, options ...Option) *Client {
	if apiKey = strings.TrimSpace(apiKey); apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("UNIFUNCS_API_KEY"))
	}
	c := &Client{
		apiKey: apiKey, http: &http.Client{}, endpoint: defaultEndpoint,
		timeout: defaultTimeout, maxRetries: defaultMaxRetries,
		retryDelay: defaultRetryDelay, maxRetryDelay: defaultMaxDelay,
		limiter: &limiter{interval: defaultInterval},
	}
	for _, option := range options {
		if option != nil {
			option(c)
		}
	}
	return c
}

// Read implements web.WebReader.
func (c *Client) Read(ctx context.Context, request web.ReadRequest) (web.Document, error) {
	markdown, stats, err := c.ReadWithStats(ctx, request)
	if err != nil {
		return web.Document{}, err
	}
	document := web.Document{
		URL: request.URL, Markdown: markdown, ContentType: "text/markdown",
		Metadata: map[string]string{
			"provider": "unifuncs", "attempts": strconv.Itoa(stats.Attempts),
			"duration_ms": strconv.FormatInt(stats.Duration.Milliseconds(), 10),
		},
	}
	if published, ok := sourcedate.ExtractPublishedDateFromMarkdown(markdown, time.Now()); ok {
		document.PublishedAt = &published.At
		document.Metadata["published_date_text"] = published.Text
		document.Metadata["published_date_source"] = published.Source
		document.Metadata["published_date_confidence"] = published.Confidence
	}
	return document, nil
}

// ReadWithStats reads a document and returns retry statistics.
func (c *Client) ReadWithStats(ctx context.Context, request web.ReadRequest) (string, ReadStats, error) {
	start := time.Now()
	stats := ReadStats{Timeout: c.timeout, MaxRetries: c.maxRetries}
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		stats.Attempts++
		content, err := c.limiter.do(ctx, func(callCtx context.Context) (string, error) {
			return c.readOnce(callCtx, request.URL)
		})
		if err == nil {
			stats.Duration = time.Since(start)
			return content, stats, nil
		}
		retry, delay := c.retryable(ctx, err, attempt)
		if !retry || attempt == c.maxRetries {
			stats.Duration = time.Since(start)
			return "", stats, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			stats.Duration = time.Since(start)
			return "", stats, ctx.Err()
		}
	}
	panic("unreachable")
}

func (c *Client) readOnce(ctx context.Context, target string) (string, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(target))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("unifuncs: absolute HTTP(S) URL is required")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{"url": target, "format": "md", "liteMode": true, "readTimeout": c.timeout.Milliseconds()})
	if err != nil {
		return "", fmt.Errorf("unifuncs: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("unifuncs: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("unifuncs: request: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("unifuncs: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", HTTPError{StatusCode: resp.StatusCode, Body: string(responseBody), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	return strings.TrimSpace(string(responseBody)), nil
}

func (c *Client) retryable(ctx context.Context, err error, attempt int) (bool, time.Duration) {
	if ctx.Err() != nil {
		return false, 0
	}
	var httpErr HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusTooManyRequests:
			if httpErr.RetryAfter > 0 {
				return true, min(httpErr.RetryAfter, c.maxRetryDelay)
			}
			return true, c.backoff(attempt)
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true, c.backoff(attempt)
		}
		return false, 0
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return true, c.backoff(attempt)
	}
	return false, 0
}

func (c *Client) backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := c.retryDelay << attempt
	return min(delay, c.maxRetryDelay)
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
}

type limiter struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

func (l *limiter) do(ctx context.Context, fn func(context.Context) (string, error)) (string, error) {
	l.mu.Lock()
	start := time.Now()
	if start.Before(l.next) {
		start = l.next
	}
	l.next = start.Add(l.interval)
	l.mu.Unlock()
	if delay := time.Until(start); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		}
	}
	return fn(ctx)
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
