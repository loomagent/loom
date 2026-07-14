package researchapp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/loomfs"
	"github.com/loomagent/loom/sourceregistry"
	"github.com/loomagent/loom/tools/web"
	"github.com/loomagent/loom/tools/web/sourcedate"
)

type searchRequest struct {
	Query string `json:"query" jsonschema:"Focused web search query." validate:"min=1,notblank"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum results, from 1 to 10. Zero uses the default of 5." validate:"omitempty,min=0,max=10"`
}

type readerRequest struct {
	URL string `json:"url" jsonschema:"Absolute HTTP or HTTPS URL to read." validate:"min=1,notblank"`
}

func newSearchTool(searcher web.WebSearcher) loom.Tool {
	params := loom.MustSchemaFor[searchRequest]()
	return loom.NewTool("web_search", "Search the web and assign stable SRC-N references to every result.", params,
		func(ctx context.Context, arguments string) (string, error) {
			input, err := loom.DecodeToolArgumentsWithSchemaFor[searchRequest]("web_search", arguments, params)
			if err != nil {
				return "", err
			}
			input.Query = strings.TrimSpace(input.Query)
			if input.Limit == 0 {
				input.Limit = 5
			}
			response, err := searcher.Search(ctx, web.SearchRequest{Query: input.Query, Limit: input.Limit})
			if err != nil {
				return "", err
			}
			if len(response.Results) > input.Limit {
				response.Results = response.Results[:input.Limit]
			}
			registry := sourceregistry.FromContext(ctx)
			if registry == nil {
				return "", errors.New("source registry missing from context")
			}
			items := make([]sourceregistry.Input, len(response.Results))
			for i, result := range response.Results {
				items[i] = sourceregistry.Input{URL: result.URL, Origin: "web_search", Title: result.Title, Snippet: result.Snippet,
					SearchDate:    sourceregistry.DateEvidence{Text: result.Metadata["date"], Source: result.Metadata["date_source"]},
					PublishedDate: sourceregistry.PublishedDate{At: result.PublishedAt}, Discovery: sourceregistry.Discovery{Tool: "web_search", Phase: "tool"}}
			}
			refs, err := registry.EnsureBatch(ctx, items)
			if err != nil {
				return "", err
			}
			session, err := loomfs.RequireTurnSession(ctx)
			if err != nil {
				return "", err
			}
			hits := make([]loomfs.SearchHit, len(response.Results))
			type resultWithRef struct {
				SrcID   string `json:"srcId"`
				Title   string `json:"title,omitempty"`
				URL     string `json:"url"`
				Snippet string `json:"snippet,omitempty"`
				Date    string `json:"date,omitempty"`
			}
			out := make([]resultWithRef, len(response.Results))
			for i, result := range response.Results {
				date := result.Metadata["date"]
				hits[i] = loomfs.SearchHit{URL: result.URL, Title: result.Title, Snippet: result.Snippet, Date: date,
					DateSource: result.Metadata["date_source"], Relevant: true, SrcID: refs[i].ID}
				out[i] = resultWithRef{SrcID: refs[i].ID, Title: result.Title, URL: result.URL, Snippet: result.Snippet, Date: date}
			}
			if _, err := session.ObserveSearch(ctx, loomfs.SearchObservation{Tool: "web_search", Phase: "tool", Query: input.Query, Hits: hits}); err != nil {
				return "", err
			}
			if err := session.Checkpoint(ctx); err != nil {
				return "", err
			}
			data, err := json.Marshal(map[string]any{"results": out})
			if err != nil {
				return "", err
			}
			return string(data), nil
		}, loom.WithRequiresNetwork())
}

func newReaderTool(reader web.WebReader) loom.Tool {
	params := loom.MustSchemaFor[readerRequest]()
	return loom.NewTool("web_reader", "Read a source, save its Markdown in the workspace, and return its stable SRC-N reference.", params,
		func(ctx context.Context, arguments string) (string, error) {
			input, err := loom.DecodeToolArgumentsWithSchemaFor[readerRequest]("web_reader", arguments, params)
			if err != nil {
				return "", err
			}
			input.URL = strings.TrimSpace(input.URL)
			document, err := reader.Read(ctx, web.ReadRequest{URL: input.URL})
			if err != nil {
				return "", err
			}
			registry := sourceregistry.FromContext(ctx)
			if registry == nil {
				return "", errors.New("source registry missing from context")
			}
			published := publicationEvidence(document)
			refs, err := registry.EnsureBatch(ctx, []sourceregistry.Input{{URL: input.URL, Origin: "web_reader", Title: document.Title,
				PublishedDate: published, Discovery: sourceregistry.Discovery{Tool: "web_reader", Phase: "tool"}, HasContent: true}})
			if err != nil {
				return "", err
			}
			session, err := loomfs.RequireTurnSession(ctx)
			if err != nil {
				return "", err
			}
			obs := loomfs.SourceObservation{Tool: "web_reader", Phase: "tool", URL: input.URL, Title: document.Title,
				Markdown: document.Markdown, SrcID: refs[0].ID, PublishedAt: published.At, PublishedDateText: published.Text,
				PublishedDateSource: published.Source, PublishedDateConfidence: published.Confidence}
			entry, _, err := session.SaveSource(ctx, obs)
			if err != nil {
				return "", err
			}
			if err := session.Checkpoint(ctx); err != nil {
				return "", err
			}
			data, err := json.Marshal(map[string]any{"srcId": refs[0].ID, "url": input.URL, "rawPath": entry.RawPath,
				"publishedAt": publishedTime(published.At), "markdown": document.Markdown})
			if err != nil {
				return "", err
			}
			return string(data), nil
		}, loom.WithRequiresNetwork())
}

func publicationEvidence(document web.Document) sourceregistry.PublishedDate {
	out := sourceregistry.PublishedDate{At: document.PublishedAt}
	if document.Metadata != nil {
		out.Text = document.Metadata["published_date_text"]
		out.Source = document.Metadata["published_date_source"]
		out.Confidence = document.Metadata["published_date_confidence"]
	}
	if out.At == nil {
		if found, ok := sourcedate.ExtractPublishedDateFromMarkdown(document.Markdown, time.Now()); ok {
			out.At, out.Text, out.Source, out.Confidence = &found.At, found.Text, found.Source, found.Confidence
		}
	}
	return out
}

func publishedTime(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}
