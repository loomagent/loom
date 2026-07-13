package proreportbench

import (
	"strings"
	"testing"
)

func TestRenderBatchMarkdownIncludesArtifactMetrics(t *testing.T) {
	report := BatchReport{
		ProReportPath: "turn.json",
		CasesRoot:     "cases",
		TotalCases:    1,
		PassCount:     1,
		ArtifactMetrics: &ArtifactMetrics{
			ArtifactDir:            "workspace/pro_report/turns/1",
			GateDecision:           "start_report",
			GateCoverageOK:         true,
			SearchQueryCount:       8,
			DeepReadCount:          17,
			IndependentDomainCount: 9,
			EvidenceRowCount:       6,
			EvidenceSourceRefCount: 18,
		},
		ReferenceProfile: ReferenceProfile{
			CaseCount: 1,
			Signals: map[Signal]SignalStats{
				SignalDataSonar: {Min: 4, Median: 5, Max: 8, Average: 5.5, Candidate: 3, DeltaFromMedian: -2, Position: "below_range"},
			},
		},
		Aggregate: AggregateDiff{AverageDelta: map[Signal]float64{}},
		Cases: []CaseReport{{
			CaseID: "case-1",
			Pass:   true,
		}},
	}
	md := RenderBatchMarkdown(report)
	for _, want := range []string{
		"## Artifact Metrics",
		"Gate decision: `start_report`",
		"Search queries: 8",
		"Deep reads: 17",
		"Evidence rows/source refs: 6/18",
		"## Unifuncs Reference Profile",
		"`data_sonar`",
		"`below_range`",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestBuildReferenceProfile(t *testing.T) {
	profile := buildReferenceProfile(map[Signal][]int{
		SignalDataSonar: {4, 8, 6},
	}, TraceSummary{Counts: map[Signal]int{SignalDataSonar: 3}})
	stat := profile.Signals[SignalDataSonar]
	if stat.Min != 4 || stat.Median != 6 || stat.Max != 8 || stat.Candidate != 3 || stat.Position != "below_range" {
		t.Fatalf("stat=%+v", stat)
	}
}
