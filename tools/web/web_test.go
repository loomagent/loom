package web

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

type fakeSearcher struct{ request SearchRequest }

func (f *fakeSearcher) Search(_ context.Context, request SearchRequest) (SearchResponse, error) {
	f.request = request
	return SearchResponse{Results: []SearchResult{{Title: "Result", URL: "https://example.com", Snippet: "text"}}}, nil
}

func TestSearchTool(t *testing.T) {
	provider := &fakeSearcher{}
	tool, err := NewSearchTool(provider, SearchToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Invoke(context.Background(), `{"query":"loom agents"}`)
	if err != nil {
		t.Fatal(err)
	}
	if provider.request.Query != "loom agents" || provider.request.Limit != 10 {
		t.Fatalf("request = %+v", provider.request)
	}
	var response SearchResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil || len(response.Results) != 1 {
		t.Fatalf("output = %s, err=%v", out, err)
	}
}

func TestSearchToolSchemaComesFromRequestStruct(t *testing.T) {
	tool, err := NewSearchTool(&fakeSearcher{}, SearchToolOptions{DefaultLimit: 3, MaxLimit: 7})
	if err != nil {
		t.Fatal(err)
	}
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(info.Parameters.Required, []string{"query"}) {
		t.Fatalf("required = %v, want [query]", info.Parameters.Required)
	}
	if info.Parameters.Properties["query"].Type != "string" || info.Parameters.Properties["limit"].Type != "integer" {
		t.Fatalf("properties = %+v", info.Parameters.Properties)
	}
	if description := info.Parameters.Properties["limit"].Description; !strings.Contains(description, "default 3, max 7") {
		t.Fatalf("limit description = %q", description)
	}
	if _, err := tool.Invoke(context.Background(), `{"query":"loom","limit":8}`); err == nil || !strings.Contains(err.Error(), `"limit" must be at most 7`) {
		t.Fatalf("out-of-range error = %v", err)
	}
}

type fakeReader struct{ request ReadRequest }

func (f *fakeReader) Read(_ context.Context, request ReadRequest) (Document, error) {
	f.request = request
	return Document{Title: "Page", Markdown: "# Page"}, nil
}

func TestReaderTool(t *testing.T) {
	provider := &fakeReader{}
	tool, err := NewReaderTool(provider, ReaderToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Invoke(context.Background(), `{"url":"https://example.com/page"}`)
	if err != nil {
		t.Fatal(err)
	}
	if provider.request.URL != "https://example.com/page" {
		t.Fatalf("request = %+v", provider.request)
	}
	var document Document
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatal(err)
	}
	if document.URL != provider.request.URL || document.Markdown != "# Page" {
		t.Fatalf("document = %+v", document)
	}
}

func TestReaderToolRejectsUnsafeSchemes(t *testing.T) {
	tool, err := NewReaderTool(&fakeReader{}, ReaderToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"/relative", "file:///etc/passwd", "javascript:alert(1)"} {
		if _, err := tool.Invoke(context.Background(), `{"url":"`+raw+`"}`); err == nil {
			t.Fatalf("expected %q to fail", raw)
		}
	}
}
