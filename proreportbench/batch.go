package proreportbench

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type BatchReport struct {
	ProReportPath    string           `json:"proreport_path"`
	CasesRoot        string           `json:"cases_root"`
	TotalCases       int              `json:"total_cases"`
	PassCount        int              `json:"pass_count"`
	BestMatch        *CaseReport      `json:"best_match,omitempty"`
	ArtifactMetrics  *ArtifactMetrics `json:"artifact_metrics,omitempty"`
	ReferenceProfile ReferenceProfile `json:"reference_profile"`
	Cases            []CaseReport     `json:"cases"`
	Aggregate        AggregateDiff    `json:"aggregate"`
}

type CaseReport struct {
	CaseID       string         `json:"case_id"`
	MarkersPath  string         `json:"markers_path"`
	Pass         bool           `json:"pass"`
	Score        int            `json:"score"`
	MissingCore  []Signal       `json:"missing_core,omitempty"`
	Forbidden    []TraceEvent   `json:"forbidden,omitempty"`
	OrderIssues  []string       `json:"order_issues,omitempty"`
	CountDeltas  []CountDelta   `json:"count_deltas,omitempty"`
	Observations []string       `json:"observations,omitempty"`
	RefCounts    map[Signal]int `json:"ref_counts,omitempty"`
}

type AggregateDiff struct {
	MissingCoreCases map[Signal]int     `json:"missing_core_cases"`
	ForbiddenCases   int                `json:"forbidden_cases"`
	OrderIssueCases  int                `json:"order_issue_cases"`
	AverageDelta     map[Signal]float64 `json:"average_delta"`
}

type ReferenceProfile struct {
	CaseCount int                    `json:"case_count"`
	Signals   map[Signal]SignalStats `json:"signals"`
}

type SignalStats struct {
	Min             int     `json:"min"`
	Median          float64 `json:"median"`
	Max             int     `json:"max"`
	Average         float64 `json:"average"`
	Candidate       int     `json:"candidate"`
	DeltaFromMedian float64 `json:"delta_from_median"`
	Position        string  `json:"position"` // below_range | within_range | above_range
}

var ProfileSignals = []Signal{
	SignalExpertResearch,
	SignalBrainstorm,
	SignalDataSonar,
	SignalDeepRead,
	SignalCrossVerify,
	SignalStartReport,
	SignalUnverifiedNotice,
	SignalVerifyCTA,
	SignalLongReport,
}

func BuildBatchReport(casesRoot, proReportPath string, candidate TraceSummary, metrics *ArtifactMetrics) (BatchReport, error) {
	markerPaths, err := FindMarkerPaths(casesRoot)
	if err != nil {
		return BatchReport{}, err
	}
	if len(markerPaths) == 0 {
		return BatchReport{}, fmt.Errorf("no step_markers.json under %s", casesRoot)
	}
	report := BatchReport{
		ProReportPath:   proReportPath,
		CasesRoot:       casesRoot,
		TotalCases:      len(markerPaths),
		ArtifactMetrics: metrics,
		Aggregate: AggregateDiff{
			MissingCoreCases: make(map[Signal]int),
			AverageDelta:     make(map[Signal]float64),
		},
	}
	deltaSums := make(map[Signal]int)
	referenceCounts := make(map[Signal][]int)
	for _, path := range markerPaths {
		ref, err := LoadUnifuncsReference(path)
		if err != nil {
			return BatchReport{}, fmt.Errorf("load unifuncs reference %s: %w", path, err)
		}
		for _, sig := range ProfileSignals {
			referenceCounts[sig] = append(referenceCounts[sig], ref.Counts[sig])
		}
		compare := Compare(ref, candidate)
		cr := CaseReport{
			CaseID:       CaseID(casesRoot, path),
			MarkersPath:  path,
			Pass:         compare.Pass,
			Score:        Score(compare),
			MissingCore:  compare.MissingCore,
			Forbidden:    compare.Forbidden,
			OrderIssues:  compare.OrderIssues,
			CountDeltas:  compare.CountDeltas,
			Observations: compare.Observations,
			RefCounts:    ref.Counts,
		}
		if cr.Pass {
			report.PassCount++
		}
		for _, sig := range cr.MissingCore {
			report.Aggregate.MissingCoreCases[sig]++
		}
		if len(cr.Forbidden) > 0 {
			report.Aggregate.ForbiddenCases++
		}
		if len(cr.OrderIssues) > 0 {
			report.Aggregate.OrderIssueCases++
		}
		for _, d := range cr.CountDeltas {
			deltaSums[d.Signal] += d.Delta
		}
		report.Cases = append(report.Cases, cr)
	}
	report.ReferenceProfile = buildReferenceProfile(referenceCounts, candidate)
	for _, sig := range CoreRequiredSignals {
		report.Aggregate.AverageDelta[sig] = float64(deltaSums[sig]) / float64(len(report.Cases))
	}
	slices.SortFunc(report.Cases, func(a, b CaseReport) int {
		if a.Score != b.Score {
			return a.Score - b.Score
		}
		return strings.Compare(a.CaseID, b.CaseID)
	})
	best := report.Cases[0]
	report.BestMatch = &best
	return report, nil
}

func LoadUnifuncsReference(path string) (TraceSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return TraceSummary{}, err
	}
	defer func() { _ = f.Close() }()
	markers, err := LoadUnifuncsMarkers(f)
	if err != nil {
		return TraceSummary{}, err
	}
	return SummarizeUnifuncs(markers), nil
}

func LoadProReportSummary(path string) (TraceSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return TraceSummary{}, err
	}
	defer func() { _ = f.Close() }()
	return SummarizeProReportJSON(f)
}

func FindMarkerPaths(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(path) != "step_markers.json" {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

func CaseID(root, markerPath string) string {
	dir := filepath.Dir(markerPath)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return filepath.Base(dir)
	}
	return filepath.ToSlash(rel)
}

func Score(report CompareReport) int {
	total := len(report.MissingCore)*1000 + len(report.Forbidden)*1000 + len(report.OrderIssues)*500
	for _, d := range report.CountDeltas {
		if d.Delta < 0 {
			total += -d.Delta * 10
		} else {
			total += d.Delta
		}
	}
	return total
}

func RenderBatchMarkdown(report BatchReport) string {
	var b strings.Builder
	b.WriteString("# ProReport vs Unifuncs Batch Compare\n\n")
	fmt.Fprintf(&b, "- ProReport trace: `%s`\n", report.ProReportPath)
	fmt.Fprintf(&b, "- Unifuncs cases: `%s`\n", report.CasesRoot)
	fmt.Fprintf(&b, "- Core pass: %d/%d\n", report.PassCount, report.TotalCases)
	if report.BestMatch != nil {
		fmt.Fprintf(&b, "- Best match: `%s` (score=%d, pass=%v)\n", report.BestMatch.CaseID, report.BestMatch.Score, report.BestMatch.Pass)
	}
	fmt.Fprintf(&b, "- Forbidden cases: %d\n", report.Aggregate.ForbiddenCases)
	fmt.Fprintf(&b, "- Order issue cases: %d\n\n", report.Aggregate.OrderIssueCases)

	if report.ArtifactMetrics != nil {
		renderArtifactMetrics(&b, *report.ArtifactMetrics)
	}

	renderReferenceProfile(&b, report.ReferenceProfile)

	b.WriteString("## Average Count Delta\n\n")
	b.WriteString("| Signal | Avg Delta |\n")
	b.WriteString("| --- | ---: |\n")
	for _, sig := range CoreRequiredSignals {
		fmt.Fprintf(&b, "| `%s` | %.2f |\n", sig, report.Aggregate.AverageDelta[sig])
	}
	b.WriteString("\n")

	if len(report.Aggregate.MissingCoreCases) > 0 {
		b.WriteString("## Missing Core Signals\n\n")
		b.WriteString("| Signal | Cases |\n")
		b.WriteString("| --- | ---: |\n")
		for _, sig := range sortedSignals(report.Aggregate.MissingCoreCases) {
			fmt.Fprintf(&b, "| `%s` | %d |\n", sig, report.Aggregate.MissingCoreCases[sig])
		}
		b.WriteString("\n")
	}

	b.WriteString("## Closest Cases\n\n")
	b.WriteString("| Case | Pass | Score | Missing | Forbidden | Order Issues |\n")
	b.WriteString("| --- | --- | ---: | ---: | ---: | ---: |\n")
	for i, c := range report.Cases {
		if i >= 8 {
			break
		}
		fmt.Fprintf(&b, "| `%s` | %v | %d | %d | %d | %d |\n",
			c.CaseID, c.Pass, c.Score, len(c.MissingCore), len(c.Forbidden), len(c.OrderIssues))
	}
	b.WriteString("\n")

	b.WriteString("## Highest Priority Differences\n\n")
	wrote := false
	diffSections := 0
	for _, c := range report.Cases {
		if c.Pass && c.Score == 0 {
			continue
		}
		wrote = true
		diffSections++
		fmt.Fprintf(&b, "### `%s`\n\n", c.CaseID)
		fmt.Fprintf(&b, "- Pass: %v\n", c.Pass)
		fmt.Fprintf(&b, "- Score: %d\n", c.Score)
		if len(c.MissingCore) > 0 {
			fmt.Fprintf(&b, "- Missing core: %s\n", signalList(c.MissingCore))
		}
		if len(c.Forbidden) > 0 {
			fmt.Fprintf(&b, "- Forbidden events: %d\n", len(c.Forbidden))
		}
		if len(c.OrderIssues) > 0 {
			fmt.Fprintf(&b, "- Order issues: %s\n", strings.Join(c.OrderIssues, "; "))
		}
		if len(c.Observations) > 0 {
			fmt.Fprintf(&b, "- Observations: %s\n", strings.Join(c.Observations, " / "))
		}
		b.WriteString("- Count deltas:")
		for _, d := range c.CountDeltas {
			fmt.Fprintf(&b, " `%s=%+d`", d.Signal, d.Delta)
		}
		b.WriteString("\n\n")
		if diffSections >= 5 {
			break
		}
	}
	if !wrote {
		b.WriteString("No high-priority differences detected by the core trace comparator.\n\n")
	}
	return b.String()
}

func MarshalJSON(v any, compact bool) ([]byte, error) {
	var data []byte
	var err error
	if compact {
		data, err = json.Marshal(v)
	} else {
		data, err = json.MarshalIndent(v, "", "  ")
	}
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func buildReferenceProfile(referenceCounts map[Signal][]int, candidate TraceSummary) ReferenceProfile {
	profile := ReferenceProfile{
		Signals: make(map[Signal]SignalStats, len(ProfileSignals)),
	}
	for _, sig := range ProfileSignals {
		values := append([]int(nil), referenceCounts[sig]...)
		if len(values) == 0 {
			continue
		}
		slices.Sort(values)
		sum := 0
		for _, v := range values {
			sum += v
		}
		median := median(values)
		candidateCount := candidate.Counts[sig]
		position := "within_range"
		if candidateCount < values[0] {
			position = "below_range"
		}
		if candidateCount > values[len(values)-1] {
			position = "above_range"
		}
		profile.CaseCount = len(values)
		profile.Signals[sig] = SignalStats{
			Min:             values[0],
			Median:          median,
			Max:             values[len(values)-1],
			Average:         float64(sum) / float64(len(values)),
			Candidate:       candidateCount,
			DeltaFromMedian: float64(candidateCount) - median,
			Position:        position,
		}
	}
	return profile
}

func median(values []int) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	mid := n / 2
	if n%2 == 1 {
		return float64(values[mid])
	}
	return float64(values[mid-1]+values[mid]) / 2
}

func renderReferenceProfile(b *strings.Builder, profile ReferenceProfile) {
	if profile.CaseCount == 0 {
		return
	}
	b.WriteString("## Unifuncs Reference Profile\n\n")
	fmt.Fprintf(b, "Reference cases: %d\n\n", profile.CaseCount)
	b.WriteString("| Signal | Min | Median | Max | Avg | Candidate | Delta Median | Position |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")
	for _, sig := range ProfileSignals {
		stat, ok := profile.Signals[sig]
		if !ok {
			continue
		}
		fmt.Fprintf(b, "| `%s` | %d | %.1f | %d | %.2f | %d | %.1f | `%s` |\n",
			sig, stat.Min, stat.Median, stat.Max, stat.Average, stat.Candidate, stat.DeltaFromMedian, stat.Position)
	}
	b.WriteString("\n")
}

func renderArtifactMetrics(b *strings.Builder, m ArtifactMetrics) {
	b.WriteString("## Artifact Metrics\n\n")
	fmt.Fprintf(b, "- Artifact dir: `%s`\n", m.ArtifactDir)
	fmt.Fprintf(b, "- Gate decision: `%s` (coverage_ok=%v)\n", m.GateDecision, m.GateCoverageOK)
	fmt.Fprintf(b, "- Search queries: %d; search batches: %d; SERP results: %d; relevant results: %d\n",
		m.SearchQueryCount, m.SearchBatchCount, m.SearchResultCount, m.RelevantResultCount)
	fmt.Fprintf(b, "- Fetch queued/fetched: %d/%d; saved sources: %d\n", m.FetchQueuedCount, m.FetchedCount, m.SavedSourceCount)
	fmt.Fprintf(b, "- Deep reads: %d; failed reads: %d; independent domains: %d\n", m.DeepReadCount, m.FailedReadCount, m.IndependentDomainCount)
	fmt.Fprintf(b, "- Coverage snapshots: %d; progress rounds: %d; open aspects at end: %d\n",
		m.CoverageSnapshotCount, m.ProgressRoundCount, m.OpenAspectCount)
	fmt.Fprintf(b, "- Evidence rows/source refs: %d/%d\n", m.EvidenceRowCount, m.EvidenceSourceRefCount)
	if m.ReportRunes > 0 {
		fmt.Fprintf(b, "- Report runes: %d\n", m.ReportRunes)
	}
	if len(m.MissingFiles) > 0 {
		fmt.Fprintf(b, "- Missing artifact files: `%s`\n", strings.Join(m.MissingFiles, "`, `"))
	}
	b.WriteString("\n")
}

func sortedSignals(values map[Signal]int) []Signal {
	out := make([]Signal, 0, len(values))
	for sig := range values {
		out = append(out, sig)
	}
	slices.Sort(out)
	return out
}

func signalList(values []Signal) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, "`"+string(v)+"`")
	}
	return strings.Join(parts, ", ")
}
