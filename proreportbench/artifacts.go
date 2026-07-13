package proreportbench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type ArtifactMetrics struct {
	ArtifactDir            string   `json:"artifact_dir"`
	SearchBatchCount       int      `json:"search_batch_count"`
	SearchQueryCount       int      `json:"search_query_count"`
	SearchResultCount      int      `json:"search_result_count"`
	RelevantResultCount    int      `json:"relevant_result_count"`
	FetchQueuedCount       int      `json:"fetch_queued_count"`
	FetchedCount           int      `json:"fetched_count"`
	SavedSourceCount       int      `json:"saved_source_count"`
	ReadingCount           int      `json:"reading_count"`
	DeepReadCount          int      `json:"deep_read_count"`
	FailedReadCount        int      `json:"failed_read_count"`
	IndependentDomainCount int      `json:"independent_domain_count"`
	CoverageSnapshotCount  int      `json:"coverage_snapshot_count"`
	ProgressRoundCount     int      `json:"progress_round_count"`
	OpenAspectCount        int      `json:"open_aspect_count"`
	EvidenceRowCount       int      `json:"evidence_row_count"`
	EvidenceSourceRefCount int      `json:"evidence_source_ref_count"`
	GateDecision           string   `json:"gate_decision,omitempty"`
	GateCoverageOK         bool     `json:"gate_coverage_ok,omitempty"`
	GateQueryCount         int      `json:"gate_query_count,omitempty"`
	GateSourceCount        int      `json:"gate_source_count,omitempty"`
	GateDeepReadCount      int      `json:"gate_deep_read_count,omitempty"`
	GateDomainCount        int      `json:"gate_domain_count,omitempty"`
	ReportRunes            int      `json:"report_runes,omitempty"`
	MissingFiles           []string `json:"missing_files,omitempty"`
}

// DiscoverArtifactDir locates the stable ProReport artifact directory for a
// database turn index inside a conversation workspace.
//
// Controller DB turns are 0-based, while loomfs TurnSession uses turn_index+1,
// so pro_report/turns/{turnIndex+1} is preferred. The direct turnIndex path is
// checked as a compatibility fallback for manually exported or test traces.
func DiscoverArtifactDir(workspaceDir string, turnIndex uint64) (string, error) {
	workspaceDir = strings.TrimSpace(workspaceDir)
	if workspaceDir == "" {
		return "", fmt.Errorf("workspace dir is required")
	}
	info, err := os.Stat(workspaceDir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", workspaceDir)
	}
	candidates := artifactDirCandidates(workspaceDir, turnIndex)
	type scored struct {
		path  string
		score int
		order int
	}
	var ranked []scored
	for i, dir := range candidates {
		score := artifactDirScore(dir)
		if score == 0 {
			continue
		}
		ranked = append(ranked, scored{path: dir, score: score, order: i})
	}
	if len(ranked) == 0 {
		return "", fmt.Errorf("no ProReport artifacts found under %s for turn index %d", workspaceDir, turnIndex)
	}
	slices.SortFunc(ranked, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return a.order - b.order
	})
	return ranked[0].path, nil
}

func artifactDirCandidates(workspaceDir string, turnIndex uint64) []string {
	nums := []uint64{turnIndex + 1, turnIndex}
	seen := map[uint64]struct{}{}
	out := make([]string, 0, len(nums))
	for _, n := range nums {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, filepath.Join(workspaceDir, "pro_report", "turns", fmt.Sprintf("%d", n)))
	}
	return out
}

func artifactDirScore(dir string) int {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return 0
	}
	score := 0
	for _, f := range []struct {
		name   string
		weight int
	}{
		{name: "start_report_gate.json", weight: 5},
		{name: "report.md", weight: 4},
		{name: "evidence_matrix.json", weight: 3},
		{name: "search_batches.jsonl", weight: 2},
		{name: "readings.jsonl", weight: 2},
		{name: "research_plan.json", weight: 1},
		{name: "context_state.json", weight: 1},
	} {
		if stat, err := os.Stat(filepath.Join(dir, f.name)); err == nil && !stat.IsDir() {
			score += f.weight
		}
	}
	return score
}

func LoadArtifactMetrics(dir string) (ArtifactMetrics, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ArtifactMetrics{}, fmt.Errorf("artifact dir is required")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return ArtifactMetrics{}, err
	}
	if !info.IsDir() {
		return ArtifactMetrics{}, fmt.Errorf("%s is not a directory", dir)
	}
	m := ArtifactMetrics{ArtifactDir: dir}
	if err := readSearchBatches(filepath.Join(dir, "search_batches.jsonl"), &m); err != nil {
		return ArtifactMetrics{}, err
	}
	if err := readReadings(filepath.Join(dir, "readings.jsonl"), &m); err != nil {
		return ArtifactMetrics{}, err
	}
	if err := readProgress(filepath.Join(dir, "progress_metrics.jsonl"), &m); err != nil {
		return ArtifactMetrics{}, err
	}
	if err := readCoverageSnapshots(filepath.Join(dir, "coverage_snapshots.jsonl"), &m); err != nil {
		return ArtifactMetrics{}, err
	}
	if err := readEvidenceMatrix(filepath.Join(dir, "evidence_matrix.json"), &m); err != nil {
		return ArtifactMetrics{}, err
	}
	if err := readStartGate(filepath.Join(dir, "start_report_gate.json"), &m); err != nil {
		return ArtifactMetrics{}, err
	}
	if err := readReportSize(filepath.Join(dir, "report.md"), &m); err != nil {
		return ArtifactMetrics{}, err
	}
	return m, nil
}

func readSearchBatches(path string, m *ArtifactMetrics) error {
	type record struct {
		Query            string   `json:"query"`
		ResultCount      int      `json:"result_count"`
		RelevantCount    int      `json:"relevant_count"`
		FetchQueuedCount int      `json:"fetch_queued_count"`
		FetchedCount     int      `json:"fetched_count"`
		SavedSourceIDs   []string `json:"saved_source_ids"`
		Duplicate        bool     `json:"duplicate"`
	}
	seenSources := map[string]struct{}{}
	return readJSONL(path, m, func(line []byte) error {
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		m.SearchBatchCount++
		if strings.TrimSpace(r.Query) != "" && !r.Duplicate {
			m.SearchQueryCount++
		}
		m.SearchResultCount += r.ResultCount
		m.RelevantResultCount += r.RelevantCount
		m.FetchQueuedCount += r.FetchQueuedCount
		m.FetchedCount += r.FetchedCount
		for _, id := range r.SavedSourceIDs {
			if id = strings.TrimSpace(id); id != "" {
				seenSources[id] = struct{}{}
			}
		}
		m.SavedSourceCount = len(seenSources)
		return nil
	})
}

func readReadings(path string, m *ArtifactMetrics) error {
	type record struct {
		SrcID      string `json:"src_id"`
		Domain     string `json:"domain"`
		HasContent bool   `json:"has_content"`
	}
	domains := map[string]struct{}{}
	return readJSONL(path, m, func(line []byte) error {
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		m.ReadingCount++
		if r.HasContent {
			m.DeepReadCount++
		} else {
			m.FailedReadCount++
		}
		if d := strings.ToLower(strings.TrimSpace(r.Domain)); d != "" && r.HasContent {
			domains[d] = struct{}{}
			m.IndependentDomainCount = len(domains)
		}
		return nil
	})
}

func readProgress(path string, m *ArtifactMetrics) error {
	type record struct {
		OpenAspectCount        int `json:"open_aspect_count"`
		IndependentDomainCount int `json:"independent_domain_count"`
	}
	return readJSONL(path, m, func(line []byte) error {
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		m.ProgressRoundCount++
		m.OpenAspectCount = r.OpenAspectCount
		if r.IndependentDomainCount > m.IndependentDomainCount {
			m.IndependentDomainCount = r.IndependentDomainCount
		}
		return nil
	})
}

func readCoverageSnapshots(path string, m *ArtifactMetrics) error {
	return readJSONL(path, m, func([]byte) error {
		m.CoverageSnapshotCount++
		return nil
	})
}

func readEvidenceMatrix(path string, m *ArtifactMetrics) error {
	type matrix struct {
		Rows []struct {
			SourceIDs []string `json:"source_ids"`
		} `json:"rows"`
	}
	var v matrix
	if err := readJSONFile(path, m, &v); err != nil {
		return err
	}
	m.EvidenceRowCount = len(v.Rows)
	for _, row := range v.Rows {
		m.EvidenceSourceRefCount += len(row.SourceIDs)
	}
	return nil
}

func readStartGate(path string, m *ArtifactMetrics) error {
	type gate struct {
		Decision               string `json:"decision"`
		CoverageOK             bool   `json:"coverage_ok"`
		QueryCount             int    `json:"query_count"`
		SourceCount            int    `json:"source_count"`
		DeepReadCount          int    `json:"deep_read_count"`
		IndependentDomainCount int    `json:"independent_domain_count"`
	}
	var v gate
	if err := readJSONFile(path, m, &v); err != nil {
		return err
	}
	m.GateDecision = strings.TrimSpace(v.Decision)
	m.GateCoverageOK = v.CoverageOK
	m.GateQueryCount = v.QueryCount
	m.GateSourceCount = v.SourceCount
	m.GateDeepReadCount = v.DeepReadCount
	m.GateDomainCount = v.IndependentDomainCount
	return nil
}

func readReportSize(path string, m *ArtifactMetrics) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			m.MissingFiles = append(m.MissingFiles, filepath.Base(path))
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	m.ReportRunes = len([]rune(strings.TrimSpace(string(data))))
	return nil
}

func readJSONFile(path string, m *ArtifactMetrics, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			m.MissingFiles = append(m.MissingFiles, filepath.Base(path))
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func readJSONL(path string, m *ArtifactMetrics, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			m.MissingFiles = append(m.MissingFiles, filepath.Base(path))
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := fn([]byte(line)); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}
	return nil
}
