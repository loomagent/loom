package sourcedate

import (
	"testing"
	"time"
)

func TestExtractPublishedDateFromMarkdownLabelled(t *testing.T) {
	now := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	got, ok := ExtractPublishedDateFromMarkdown(`# Report

Published: Friday, May 30, 2025

Body mentions January 1, 2020 later.
`, now)
	if !ok {
		t.Fatalf("expected publication date")
	}
	if got.At.Format(time.DateOnly) != "2025-05-30" {
		t.Fatalf("date = %s", got.At.Format(time.DateOnly))
	}
	if got.Text != "Friday, May 30, 2025" || got.Source != ReaderMarkdownLabelledDate || got.Confidence != ConfidenceHigh {
		t.Fatalf("metadata = %+v", got)
	}
}

func TestExtractPublishedDateFromMarkdownTopline(t *testing.T) {
	now := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	got, ok := ExtractPublishedDateFromMarkdown(`# Justice Department

Friday, May 30, 2025

The Department announced...
`, now)
	if !ok {
		t.Fatalf("expected top-line publication date")
	}
	if got.At.Format(time.DateOnly) != "2025-05-30" || got.Source != ReaderMarkdownToplineDate || got.Confidence != ConfidenceMedium {
		t.Fatalf("metadata = %+v", got)
	}
}

func TestExtractPublishedDateFromMarkdownFormats(t *testing.T) {
	now := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		markdown string
		want     string
	}{
		{name: "ISO", markdown: "# Report\n\nPublished: 2025-05-30", want: "2025-05-30"},
		{name: "slash", markdown: "# Report\n\nPosted: 2025/5/30", want: "2025-05-30"},
		{name: "Chinese", markdown: "# 报告\n\nDate: 2025年5月30日", want: "2025-05-30"},
		{name: "day month", markdown: "# Report\n\nIssued: 30 May 2025", want: "2025-05-30"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ExtractPublishedDateFromMarkdown(tt.markdown, now)
			if !ok {
				t.Fatal("expected publication date")
			}
			if date := got.At.Format(time.DateOnly); date != tt.want {
				t.Fatalf("date = %s, want %s", date, tt.want)
			}
		})
	}
}

func TestExtractPublishedDateFromMarkdownRejectsImplausibleDates(t *testing.T) {
	now := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	for _, markdown := range []string{
		"Published: January 1, 1989",
		"Published: June 27, 2026",
	} {
		if got, ok := ExtractPublishedDateFromMarkdown(markdown, now); ok {
			t.Fatalf("unexpected publication date: %+v", got)
		}
	}
}

func TestExtractPublishedDateFromMarkdownDoesNotScanBody(t *testing.T) {
	now := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	_, ok := ExtractPublishedDateFromMarkdown(`# Historical timeline

This article discusses milestones.

`+manyLines(35)+`
May 30, 2025 was one of the referenced events.
`, now)
	if ok {
		t.Fatalf("body date should not be treated as publication date")
	}
}

func manyLines(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += "body line\n"
	}
	return out
}
