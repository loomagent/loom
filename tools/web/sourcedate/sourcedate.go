// Package sourcedate extracts conservative publication-date evidence from
// Markdown document headers.
package sourcedate

import (
	"regexp"
	"strings"
	"time"
)

const (
	ReaderMarkdownLabelledDate = "reader.markdown.labelled_date"
	ReaderMarkdownToplineDate  = "reader.markdown.topline_date"
)

const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
)

type PublishedDate struct {
	At         time.Time
	Text       string
	Source     string
	Confidence string
}

var (
	labelledDateRE = regexp.MustCompile(`(?i)\b(?:published|posted|release\s+date|issued|date)\b\s*(?:on|:|-|—)?\s*(.+)$`)
	monthDateRE    = regexp.MustCompile(`(?i)(?:Mon(?:day)?|Tue(?:sday)?|Wed(?:nesday)?|Thu(?:rsday)?|Fri(?:day)?|Sat(?:urday)?|Sun(?:day)?)?,?\s*(?:Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|Jul(?:y)?|Aug(?:ust)?|Sep(?:t(?:ember)?)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)\.?\s+\d{1,2},?\s+\d{4}`)
	dayMonthDateRE = regexp.MustCompile(`(?i)\d{1,2}\s+(?:Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|Jul(?:y)?|Aug(?:ust)?|Sep(?:t(?:ember)?)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)\.?\s+\d{4}`)
	isoDateRE      = regexp.MustCompile(`\b\d{4}[-/]\d{1,2}[-/]\d{1,2}\b`)
	chineseDateRE  = regexp.MustCompile(`(\d{4})年(\d{1,2})月(\d{1,2})日`)
)

// ExtractPublishedDateFromMarkdown extracts conservative publication-date evidence
// from reader output. It intentionally only inspects the document header so body
// dates, related-story snippets, and historical timelines do not become source dates.
func ExtractPublishedDateFromMarkdown(markdown string, now time.Time) (PublishedDate, bool) {
	lines := headerLines(markdown, 80)
	for _, line := range lines {
		if match := labelledDateRE.FindStringSubmatch(line); len(match) == 2 {
			if got, ok := parseDateCandidate(match[1], ReaderMarkdownLabelledDate, ConfidenceHigh, now); ok {
				return got, true
			}
		}
	}

	for i, line := range lines {
		if i >= 30 {
			break
		}
		if got, ok := parseDateCandidate(line, ReaderMarkdownToplineDate, ConfidenceMedium, now); ok && looksStandaloneDateLine(line, got.Text) {
			return got, true
		}
	}
	return PublishedDate{}, false
}

func headerLines(markdown string, limit int) []string {
	rawLines := strings.Split(markdown, "\n")
	out := make([]string, 0, limit)
	for _, raw := range rawLines {
		line := cleanMarkdownLine(raw)
		if line == "" {
			continue
		}
		out = append(out, line)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func cleanMarkdownLine(line string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimLeft(line, "#>*- \t")
	line = strings.Trim(line, "`*_ ")
	line = strings.Join(strings.Fields(line), " ")
	return line
}

func parseDateCandidate(text, source, confidence string, now time.Time) (PublishedDate, bool) {
	candidate := extractDateText(text)
	if candidate == "" {
		return PublishedDate{}, false
	}
	at, ok := parseDateText(candidate)
	if !ok || !plausibleDate(at, now) {
		return PublishedDate{}, false
	}
	return PublishedDate{
		At:         at,
		Text:       candidate,
		Source:     source,
		Confidence: confidence,
	}, true
}

func extractDateText(text string) string {
	text = strings.TrimSpace(text)
	for _, re := range []*regexp.Regexp{chineseDateRE, monthDateRE, dayMonthDateRE, isoDateRE} {
		if got := re.FindString(text); strings.TrimSpace(got) != "" {
			return strings.Trim(got, " ,.;")
		}
	}
	return ""
}

func parseDateText(text string) (time.Time, bool) {
	if match := chineseDateRE.FindStringSubmatch(text); len(match) == 4 {
		t, err := time.ParseInLocation("2006年1月2日", match[0], time.UTC)
		return t, err == nil
	}

	normalized := strings.ReplaceAll(text, ".", "")
	normalized = strings.Join(strings.Fields(strings.TrimSpace(normalized)), " ")
	layouts := []string{
		"Monday, January 2, 2006",
		"Monday, Jan 2, 2006",
		"Mon, January 2, 2006",
		"Mon, Jan 2, 2006",
		"January 2, 2006",
		"Jan 2, 2006",
		"January 2 2006",
		"Jan 2 2006",
		"2 January 2006",
		"2 Jan 2006",
		"2006-1-2",
		"2006-01-02",
		"2006/1/2",
		"2006/01/02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, normalized, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func plausibleDate(at time.Time, now time.Time) bool {
	if at.IsZero() || at.Year() < 1990 {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	return !at.After(now.Add(24 * time.Hour))
}

func looksStandaloneDateLine(line, dateText string) bool {
	rest := strings.TrimSpace(strings.Replace(line, dateText, "", 1))
	rest = strings.Trim(rest, " ,.;|-—")
	if rest == "" {
		return true
	}
	words := strings.Fields(rest)
	return len(words) <= 2 && !strings.Contains(strings.ToLower(rest), "related")
}
