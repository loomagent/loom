package loomfs

import (
	"strings"
	"testing"
	"time"
)

func TestRenderContextBlockIncludesSourceDateEvidence(t *testing.T) {
	published := time.Date(2025, 5, 30, 0, 0, 0, 0, time.UTC)
	got := RenderContextBlock(ContextSnapshot{
		Sources: []SourceEntry{{
			ID:                      "SRC-1",
			Tier:                    "official",
			Domain:                  "justice.gov",
			RawPath:                 "raw/SRC-1.md",
			PublishedAt:             &published,
			PublishedDateText:       "May 30, 2025",
			PublishedDateSource:     "reader.markdown.labelled_date",
			PublishedDateConfidence: "high",
			Summary:                 "DOJ announcement.",
		}},
	}, RenderOptions{})

	if !strings.Contains(got, "SRC-1 | official | justice.gov | published=2025-05-30") {
		t.Fatalf("RenderContextBlock missing source date evidence:\n%s", got)
	}
}
