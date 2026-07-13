package sourceregistry

import "testing"

func TestMergeSourceRepairsContentPathAndKeepsEvidenceAtomic(t *testing.T) {
	existing := Source{Seq: 7, URL: "https://example.com", HasContent: true, SearchDate: DateEvidence{Text: "old", Source: "old-source"}}
	got, changed := MergeSource(existing, Candidate{Key: existing.URL, Input: Input{SearchDate: DateEvidence{Text: "new", Source: "new-source"}}})
	if !changed || got.RawPath != "raw/SRC-7.md" {
		t.Fatalf("merge=%+v changed=%v", got, changed)
	}
	if got.SearchDate.Text != "old" || got.SearchDate.Source != "old-source" {
		t.Fatalf("evidence mixed: %+v", got.SearchDate)
	}
}
