package loomfs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/loomagent/loom"
)

func TestTurnSessionMaterializesSearchSourceLinks(t *testing.T) {
	root := t.TempDir()
	ws, err := OpenWorkspace(root)
	if err != nil {
		t.Fatalf("OpenWorkspace: %v", err)
	}
	session, err := ws.BeginTurn(TurnMeta{ConversationID: "conv_1", TurnIndex: 1, ChatModeID: "智能搜索", Executor: "react", UserText: "search"})
	if err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	rec, err := session.ObserveSearch(context.Background(), SearchObservation{
		Tool:  "web_search",
		Phase: "tool",
		Query: "spacex ipo",
		Hits: []SearchHit{{
			URL:        "https://example.com/a",
			Title:      "A",
			Date:       "May 30, 2025",
			DateSource: "serper.organic.date",
			Relevant:   true,
		}},
	})
	if err != nil {
		t.Fatalf("ObserveSearch: %v", err)
	}
	if rec.QueryID != "QUERY-1" || rec.TurnIndex != 1 || rec.TurnQueryIndex != 1 {
		t.Fatalf("query metadata = %+v", rec)
	}
	if rec.Hits[0].Date != "May 30, 2025" || rec.Hits[0].DateSource != "serper.organic.date" {
		t.Fatalf("query date metadata = %+v", rec.Hits[0])
	}
	entry, added, err := session.SaveSource(context.Background(), SourceObservation{
		Tool:     "web_reader",
		Phase:    "tool",
		URL:      "https://example.com/a#frag",
		Markdown: "# SpaceX IPO\n\nPricing details.",
		Summary:  "Pricing details.",
		Tier:     "official",
	})
	if err != nil {
		t.Fatalf("SaveSource: %v", err)
	}
	if !added {
		t.Fatalf("SaveSource added = false, want true")
	}
	if entry.ID != "SRC-1" || entry.FoundByQuery != "QUERY-1" || entry.RawPath != "raw/SRC-1.md" {
		t.Fatalf("source entry = %+v", entry)
	}
	if entry.Date != "May 30, 2025" || entry.DateSource != "serper.organic.date" {
		t.Fatalf("source date metadata = %+v", entry)
	}
	if err := session.Checkpoint(context.Background()); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	queries, err := LoadQueryRecords(root)
	if err != nil {
		t.Fatalf("LoadQueryRecords: %v", err)
	}
	if len(queries) != 1 || queries[0].QueryID != "QUERY-1" || queries[0].Hits[0].SrcID != "SRC-1" || queries[0].NumSaved != 1 {
		t.Fatalf("queries = %+v", queries)
	}
	sources, err := NewCorpus(root)
	if err != nil {
		t.Fatalf("NewCorpus: %v", err)
	}
	entries, err := sources.LoadSources()
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "SRC-1" || entries[0].FoundByQuery != "QUERY-1" {
		t.Fatalf("sources = %+v", entries)
	}
	if entries[0].Date != "May 30, 2025" || entries[0].DateSource != "serper.organic.date" {
		t.Fatalf("persisted source date metadata = %+v", entries[0])
	}
	raw, err := os.ReadFile(filepath.Join(root, "raw", "SRC-1.md"))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !strings.Contains(string(raw), "Pricing details") {
		t.Fatalf("raw missing content: %s", string(raw))
	}
}

func TestTurnSessionGlobalIDsAndPriorTurns(t *testing.T) {
	root := t.TempDir()
	ws, err := OpenWorkspace(root)
	if err != nil {
		t.Fatalf("OpenWorkspace: %v", err)
	}

	first, err := ws.BeginTurn(TurnMeta{ConversationID: "conv_1", TurnIndex: 1, ChatModeID: "智能搜索", Executor: "react", UserText: "first"})
	if err != nil {
		t.Fatalf("BeginTurn first: %v", err)
	}
	if _, err := first.ObserveSearch(context.Background(), SearchObservation{Query: "first query", Hits: []SearchHit{{URL: "https://example.com/1", Relevant: true}}}); err != nil {
		t.Fatalf("ObserveSearch first: %v", err)
	}
	if _, _, err := first.SaveSource(context.Background(), SourceObservation{URL: "https://example.com/1", Markdown: "first raw body", Summary: "first source"}); err != nil {
		t.Fatalf("SaveSource first: %v", err)
	}
	if err := first.Finish(context.Background(), TurnOutcome{Turn: completedTurn(0, "first answer")}); err != nil {
		t.Fatalf("Finish first: %v", err)
	}

	second, err := ws.BeginTurn(TurnMeta{ConversationID: "conv_1", TurnIndex: 2, ChatModeID: "专业报告", Executor: "pro_report", UserText: "second"})
	if err != nil {
		t.Fatalf("BeginTurn second: %v", err)
	}
	reportPath := filepath.Join(root, "pro_report", "turns", "2", "report.md")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		t.Fatalf("create report dir: %v", err)
	}
	if err := os.WriteFile(reportPath, []byte("second report"), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	if err := second.RecordArtifact("pro_report/turns/2/report.md"); err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	rec, err := second.ObserveSearch(context.Background(), SearchObservation{Query: "second query", Hits: []SearchHit{{URL: "https://example.com/2", Relevant: true}}})
	if err != nil {
		t.Fatalf("ObserveSearch second: %v", err)
	}
	if rec.QueryID != "QUERY-2" || rec.TurnIndex != 2 {
		t.Fatalf("second query = %+v", rec)
	}
	if err := second.Finish(context.Background(), TurnOutcome{Turn: completedTurn(1, "second answer")}); err != nil {
		t.Fatalf("Finish second: %v", err)
	}

	turns, err := LoadPriorTurns(root)
	if err != nil {
		t.Fatalf("LoadPriorTurns: %v", err)
	}
	if len(turns) != 2 || turns[0].TurnIndex != 1 || turns[1].TurnIndex != 2 || turns[1].FinalAnswer != "second answer" {
		t.Fatalf("prior turns = %+v", turns)
	}
	if turns[0].FinalAnswerPath != "turns/1/final.md" || turns[1].FinalAnswerPath != "turns/2/final.md" {
		t.Fatalf("final answer paths = %q, %q", turns[0].FinalAnswerPath, turns[1].FinalAnswerPath)
	}
	finalBytes, err := os.ReadFile(filepath.Join(root, turns[1].FinalAnswerPath))
	if err != nil {
		t.Fatalf("read final answer artifact: %v", err)
	}
	if string(finalBytes) != "second answer\n" {
		t.Fatalf("final artifact = %q", string(finalBytes))
	}
	snapshot, err := ws.LoadSnapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(snapshot.TurnContexts) != 2 {
		t.Fatalf("turn contexts = %+v, want 2", snapshot.TurnContexts)
	}
	firstCtx := snapshot.TurnContexts[0]
	if firstCtx.TurnIndex != 1 || firstCtx.FinalSummary != "first answer" {
		t.Fatalf("first turn context = %+v", firstCtx)
	}
	if len(firstCtx.QueryIDs) != 1 || firstCtx.QueryIDs[0] != "QUERY-1" {
		t.Fatalf("first turn context query IDs = %+v", firstCtx.QueryIDs)
	}
	if len(firstCtx.SourceIDs) != 1 || firstCtx.SourceIDs[0] != "SRC-1" {
		t.Fatalf("first turn context source IDs = %+v", firstCtx.SourceIDs)
	}
	wantArtifacts := []string{"turns/1/final.md", "raw/SRC-1.md"}
	for _, want := range wantArtifacts {
		if !hasString(firstCtx.ArtifactPaths, want) {
			t.Fatalf("first turn context artifacts = %+v, missing %s", firstCtx.ArtifactPaths, want)
		}
	}
	secondCtx := snapshot.TurnContexts[1]
	if !hasString(secondCtx.ArtifactPaths, "pro_report/turns/2/report.md") {
		t.Fatalf("second turn context artifacts = %+v, missing report artifact", secondCtx.ArtifactPaths)
	}
	if _, err := os.Stat(filepath.Join(root, contextDirName, turnContextsFile)); err != nil {
		t.Fatalf("turn contexts view missing: %v", err)
	}
	loadedContexts, err := LoadTurnContexts(root)
	if err != nil {
		t.Fatalf("LoadTurnContexts: %v", err)
	}
	if len(loadedContexts) != 2 || loadedContexts[0].TurnIndex != 1 {
		t.Fatalf("loaded turn contexts = %+v", loadedContexts)
	}
	queries, err := LoadQueryRecords(root)
	if err != nil {
		t.Fatalf("LoadQueryRecords: %v", err)
	}
	if len(queries) != 2 || queries[0].QueryID != "QUERY-1" || queries[1].QueryID != "QUERY-2" {
		t.Fatalf("queries = %+v", queries)
	}
}

func TestLaterSearchLinksExistingSourceByURL(t *testing.T) {
	root := t.TempDir()
	ws, err := OpenWorkspace(root)
	if err != nil {
		t.Fatalf("OpenWorkspace: %v", err)
	}
	first, err := ws.BeginTurn(TurnMeta{ConversationID: "conv_1", TurnIndex: 1, Executor: "react"})
	if err != nil {
		t.Fatalf("BeginTurn first: %v", err)
	}
	if _, _, err := first.SaveSource(context.Background(), SourceObservation{URL: "https://example.com/a", Markdown: "body"}); err != nil {
		t.Fatalf("SaveSource first: %v", err)
	}
	if err := first.Checkpoint(context.Background()); err != nil {
		t.Fatalf("Checkpoint first: %v", err)
	}

	second, err := ws.BeginTurn(TurnMeta{ConversationID: "conv_1", TurnIndex: 2, Executor: "react"})
	if err != nil {
		t.Fatalf("BeginTurn second: %v", err)
	}
	rec, err := second.ObserveSearch(context.Background(), SearchObservation{Query: "same", Hits: []SearchHit{{URL: "https://example.com/a#x", Relevant: true}}})
	if err != nil {
		t.Fatalf("ObserveSearch second: %v", err)
	}
	if len(rec.Hits) != 1 || rec.Hits[0].SrcID != "SRC-1" {
		t.Fatalf("search did not link existing source: %+v", rec)
	}
}

func completedTurn(index uint64, answer string) *loom.Turn {
	return &loom.Turn{
		Index:  index,
		Status: loom.TurnStatusCompleted,
		Items: []loom.Item{{
			Kind:      loom.ItemKindFinalAnswer,
			Status:    loom.ItemStatusCompleted,
			Text:      answer,
			UpdatedAt: time.Now(),
		}},
		CloseReason: &loom.CloseReason{Code: loom.CloseCodeFinalAnswer},
		UpdatedAt:   time.Now(),
	}
}

func hasString(values []string, want string) bool {
	return slices.Contains(values, want)
}
