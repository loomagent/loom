package loomfs

import (
	"fmt"
	"strings"
)

const unknownSourceDateLabel = "date=—"

// SourceDateLabel returns the compact date evidence shown in source navigation.
// Verified page publication metadata wins over search-engine date hints.
func SourceDateLabel(src SourceEntry) string {
	if src.PublishedAt != nil {
		parts := []string{"published=" + src.PublishedAt.Format("2006-01-02")}
		if text := strings.TrimSpace(src.PublishedDateText); text != "" {
			parts = append(parts, "text="+quoteSourceDatePart(text))
		}
		if source := strings.TrimSpace(src.PublishedDateSource); source != "" {
			parts = append(parts, "source="+source)
		}
		if confidence := strings.TrimSpace(src.PublishedDateConfidence); confidence != "" {
			parts = append(parts, "confidence="+confidence)
		}
		return strings.Join(parts, " ")
	}
	if date := strings.TrimSpace(src.Date); date != "" {
		parts := []string{"serp_date=" + quoteSourceDatePart(date)}
		if source := strings.TrimSpace(src.DateSource); source != "" {
			parts = append(parts, "source="+source)
		}
		return strings.Join(parts, " ")
	}
	return unknownSourceDateLabel
}

func quoteSourceDatePart(v string) string {
	if strings.ContainsAny(v, " \t|") {
		return fmt.Sprintf("%q", v)
	}
	return v
}
