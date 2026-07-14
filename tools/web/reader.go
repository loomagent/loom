// Package web provides provider-neutral web search and document reader tools.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/loomagent/loom"
)

// ToolNameReader is the default name exposed by the web reader tool.
const ToolNameReader = "web_reader"

type readerToolRequest struct {
	URL string `json:"url" jsonschema:"Absolute HTTP or HTTPS URL to read." validate:"min=1,notblank" example:"https://example.com/article"`
}

// WebReader fetches and normalizes one web document.
type WebReader interface {
	Read(ctx context.Context, request ReadRequest) (Document, error)
}

// ReadRequest describes a provider-independent document read.
type ReadRequest struct {
	URL string
}

// Document is normalized web content. Markdown is the canonical body passed to
// models; providers may attach additional non-secret metadata.
type Document struct {
	URL         string            `json:"url"`
	FinalURL    string            `json:"finalUrl,omitempty"`
	Title       string            `json:"title,omitempty"`
	Markdown    string            `json:"markdown"`
	ContentType string            `json:"contentType,omitempty"`
	PublishedAt *time.Time        `json:"publishedAt,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// ReaderToolOptions configures NewReaderTool.
type ReaderToolOptions struct {
	Name        string
	Description string
}

// NewReaderTool exposes WebReader as a Loom tool. It accepts only absolute HTTP(S)
// URLs; network policy, authentication, caching, and persistence remain the
// WebReader implementation's responsibility.
func NewReaderTool(reader WebReader, options ReaderToolOptions) (loom.Tool, error) {
	if reader == nil {
		return nil, errors.New("web: WebReader is required")
	}
	name := options.Name
	if name == "" {
		name = ToolNameReader
	}
	description := strings.TrimSpace(options.Description)
	if description == "" {
		description = "Read an HTTP or HTTPS URL and return normalized Markdown content and document metadata."
	}
	contract, err := loom.NewToolContract[readerToolRequest](name)
	if err != nil {
		return nil, err
	}
	return loom.NewTool(contract, description, func(ctx context.Context, input readerToolRequest) (string, error) {
		input.URL = strings.TrimSpace(input.URL)
		parsed, err := url.ParseRequestURI(input.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return "", fmt.Errorf("web reader: absolute HTTP(S) URL is required")
		}
		document, err := reader.Read(ctx, ReadRequest{URL: input.URL})
		if err != nil {
			return "", fmt.Errorf("web reader: %w", err)
		}
		if document.URL == "" {
			document.URL = input.URL
		}
		data, err := json.Marshal(document)
		if err != nil {
			return "", fmt.Errorf("web reader: marshal document: %w", err)
		}
		return string(data), nil
	}, loom.WithRequiresNetwork()), nil
}
