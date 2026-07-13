package researchapp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/loomagent/loom/loomfs"
	"github.com/loomagent/loom/sourceregistry"
	"github.com/loomagent/loom/tools/web"
)

type fakeSearcher struct{}

func (fakeSearcher) Search(_ context.Context, request web.SearchRequest) (web.SearchResponse, error) {
	return web.SearchResponse{Results: []web.SearchResult{{
		Title: "Evidence", URL: "https://EXAMPLE.com/report#search", Snippet: request.Query,
	}}}, nil
}

type fakeReader struct{}

func (fakeReader) Read(_ context.Context, request web.ReadRequest) (web.Document, error) {
	return web.Document{URL: request.URL, Title: "Evidence", Markdown: "# Evidence\n\nPublished: May 30, 2025\n\nFull text."}, nil
}

func TestResearchToolsKeepStableReferencesAcrossTurns(t *testing.T) {
	workspace, err := loomfs.OpenWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := sourceregistry.New("conversation", sourceregistry.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	search := newSearchTool(fakeSearcher{})
	reader := newReaderTool(fakeReader{})

	first, err := workspace.BeginTurn(loomfs.TurnMeta{ConversationID: "conversation", TurnIndex: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := sourceregistry.WithContext(loomfs.WithTurnSession(context.Background(), first), registry)
	searchOutput, err := search.Invoke(ctx, `{"query":"first query","limit":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if sourceID(t, searchOutput) != "SRC-1" {
		t.Fatalf("search output = %s", searchOutput)
	}
	readOutput, err := reader.Invoke(ctx, `{"url":"https://example.com/report#reader"}`)
	if err != nil {
		t.Fatal(err)
	}
	if sourceID(t, readOutput) != "SRC-1" {
		t.Fatalf("reader output = %s", readOutput)
	}
	if err := first.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}

	second, err := workspace.BeginTurn(loomfs.TurnMeta{ConversationID: "conversation", TurnIndex: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx = sourceregistry.WithContext(loomfs.WithTurnSession(context.Background(), second), registry)
	secondOutput, err := search.Invoke(ctx, `{"query":"follow up","limit":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if sourceID(t, secondOutput) != "SRC-1" {
		t.Fatalf("second search output = %s", secondOutput)
	}

	snapshot, err := workspace.LoadSnapshot(context.Background(), loomfs.SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sources) != 1 || snapshot.Sources[0].ID != "SRC-1" || snapshot.Sources[0].RawPath != "raw/SRC-1.md" {
		t.Fatalf("sources = %+v", snapshot.Sources)
	}
	if len(snapshot.Queries) != 2 || snapshot.Queries[0].Hits[0].SrcID != "SRC-1" || snapshot.Queries[1].Hits[0].SrcID != "SRC-1" {
		t.Fatalf("queries = %+v", snapshot.Queries)
	}
}

func sourceID(t *testing.T, raw string) string {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		t.Fatal(err)
	}
	if id, _ := object["srcId"].(string); id != "" {
		return id
	}
	results, _ := object["results"].([]any)
	if len(results) == 0 {
		return ""
	}
	result, _ := results[0].(map[string]any)
	id, _ := result["srcId"].(string)
	return id
}
