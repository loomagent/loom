package proreportbench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadArtifactMetrics(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("search_batches.jsonl", `{"query":"alpha","result_count":10,"relevant_count":8,"fetch_queued_count":4,"fetched_count":4,"saved_source_ids":["SRC-1","SRC-2"],"duplicate":false}
{"query":"alpha","duplicate":true}
`)
	write("readings.jsonl", `{"src_id":"SRC-1","domain":"a.example","has_content":true}
{"src_id":"SRC-2","domain":"b.example","has_content":true}
{"url":"https://bad.example","domain":"bad.example","has_content":false}
`)
	write("progress_metrics.jsonl", `{"round":1,"open_aspect_count":2,"independent_domain_count":2}
{"round":2,"open_aspect_count":0,"independent_domain_count":2}
`)
	write("coverage_snapshots.jsonl", `{"round":1}
{"round":2}
`)
	write("evidence_matrix.json", `{"rows":[{"source_ids":["SRC-1","SRC-2"]},{"source_ids":["SRC-2"]}]}`)
	write("start_report_gate.json", `{"decision":"start_report","coverage_ok":true,"query_count":1,"source_count":2,"deep_read_count":2,"independent_domain_count":2}`)
	write("report.md", "## Report\n\nbody")

	got, err := LoadArtifactMetrics(root)
	if err != nil {
		t.Fatalf("LoadArtifactMetrics: %v", err)
	}
	if got.SearchBatchCount != 2 || got.SearchQueryCount != 1 {
		t.Fatalf("search counts=%+v", got)
	}
	if got.DeepReadCount != 2 || got.FailedReadCount != 1 || got.IndependentDomainCount != 2 {
		t.Fatalf("read counts=%+v", got)
	}
	if got.ProgressRoundCount != 2 || got.OpenAspectCount != 0 || got.CoverageSnapshotCount != 2 {
		t.Fatalf("progress counts=%+v", got)
	}
	if got.EvidenceRowCount != 2 || got.EvidenceSourceRefCount != 3 {
		t.Fatalf("evidence counts=%+v", got)
	}
	if got.GateDecision != "start_report" || !got.GateCoverageOK || got.GateDeepReadCount != 2 {
		t.Fatalf("gate=%+v", got)
	}
	if got.ReportRunes == 0 {
		t.Fatalf("missing report size: %+v", got)
	}
}

func TestDiscoverArtifactDirPrefersControllerSessionIndex(t *testing.T) {
	root := t.TempDir()
	compatDir := filepath.Join(root, "pro_report", "turns", "0")
	sessionDir := filepath.Join(root, "pro_report", "turns", "1")
	if err := os.MkdirAll(compatDir, 0o755); err != nil {
		t.Fatalf("mkdir compat: %v", err)
	}
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(compatDir, "report.md"), []byte("compat"), 0o644); err != nil {
		t.Fatalf("write compat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "start_report_gate.json"), []byte(`{"decision":"start_report"}`), 0o644); err != nil {
		t.Fatalf("write session gate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "report.md"), []byte("session"), 0o644); err != nil {
		t.Fatalf("write session report: %v", err)
	}

	got, err := DiscoverArtifactDir(root, 0)
	if err != nil {
		t.Fatalf("DiscoverArtifactDir: %v", err)
	}
	if got != sessionDir {
		t.Fatalf("artifact dir = %q, want %q", got, sessionDir)
	}
}

func TestDiscoverArtifactDirFallsBackToDirectIndex(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pro_report", "turns", "3")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("report"), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}

	got, err := DiscoverArtifactDir(root, 3)
	if err != nil {
		t.Fatalf("DiscoverArtifactDir: %v", err)
	}
	if got != dir {
		t.Fatalf("artifact dir = %q, want %q", got, dir)
	}
}
