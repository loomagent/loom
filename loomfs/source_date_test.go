package loomfs

import (
	"strings"
	"testing"
	"time"
)

func TestSourceDateLabelPrefersPublishedDate(t *testing.T) {
	published := time.Date(2025, 5, 30, 0, 0, 0, 0, time.UTC)
	got := SourceDateLabel(SourceEntry{
		Date:                    "May 31, 2025",
		DateSource:              "serper.organic.date",
		PublishedAt:             &published,
		PublishedDateText:       "May 30, 2025",
		PublishedDateSource:     "reader.markdown.labelled_date",
		PublishedDateConfidence: "high",
	})
	for _, want := range []string{
		"published=2025-05-30",
		`text="May 30, 2025"`,
		"source=reader.markdown.labelled_date",
		"confidence=high",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("SourceDateLabel missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "serp_date") {
		t.Fatalf("published date should win over serp date: %s", got)
	}
}

func TestSourceDateLabelFallsBackToSerpDate(t *testing.T) {
	got := SourceDateLabel(SourceEntry{Date: "May 30, 2025", DateSource: "serper.organic.date"})
	if got != `serp_date="May 30, 2025" source=serper.organic.date` {
		t.Fatalf("SourceDateLabel = %q", got)
	}
}
