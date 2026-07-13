package sourceregistry

import "strings"

// NewSource constructs a canonical record for a newly allocated sequence.
// Store implementations should use this helper to keep derived fields
// consistent.
func NewSource(seq uint64, candidate Candidate) Source {
	source, _ := MergeSource(Source{Seq: seq}, candidate)
	return source
}

// MergeSource fills missing metadata and upgrades HasContent monotonically.
// Existing non-empty values win, preserving the first accepted provenance.
// SearchDate and PublishedDate are atomic evidence bundles and are never mixed
// field-by-field across observations.
func MergeSource(existing Source, candidate Candidate) (Source, bool) {
	before := existing
	in := candidate.Input
	existing.URL = first(existing.URL, candidate.Key)
	existing.OriginalURL = first(existing.OriginalURL, in.OriginalURL)
	existing.Domain = first(existing.Domain, Domain(candidate.Key))
	existing.Origin = first(existing.Origin, in.Origin)
	existing.Title = first(existing.Title, in.Title)
	existing.Snippet = first(existing.Snippet, in.Snippet)
	if existing.SearchDate.Text == "" && strings.TrimSpace(in.SearchDate.Text) != "" {
		existing.SearchDate = cleanDateEvidence(in.SearchDate)
	}
	if existing.PublishedDate.At == nil && existing.PublishedDate.Text == "" &&
		(in.PublishedDate.At != nil || strings.TrimSpace(in.PublishedDate.Text) != "") {
		existing.PublishedDate = clonePublishedDate(in.PublishedDate)
	}
	existing.Summary = first(existing.Summary, in.Summary)
	existing.Tier = first(existing.Tier, in.Tier)
	if discoveryEmpty(existing.Discovery) && !discoveryEmpty(in.Discovery) {
		existing.Discovery = cleanDiscovery(in.Discovery)
	}
	if in.HasContent && !existing.HasContent {
		existing.HasContent = true
	}
	if existing.HasContent && existing.RawPath == "" {
		existing.RawPath = RawPath(existing.Seq)
	}
	return existing, existing != before
}

func first(existing, incoming string) string {
	if strings.TrimSpace(existing) != "" {
		return existing
	}
	return strings.TrimSpace(incoming)
}

func cleanDateEvidence(value DateEvidence) DateEvidence {
	return DateEvidence{Text: strings.TrimSpace(value.Text), Source: strings.TrimSpace(value.Source)}
}

func clonePublishedDate(value PublishedDate) PublishedDate {
	out := PublishedDate{Text: strings.TrimSpace(value.Text), Source: strings.TrimSpace(value.Source), Confidence: strings.TrimSpace(value.Confidence)}
	if value.At != nil {
		at := *value.At
		out.At = &at
	}
	return out
}

func discoveryEmpty(value Discovery) bool {
	return value.TurnIndex == 0 && value.Round == 0 && strings.TrimSpace(value.Tool) == "" && strings.TrimSpace(value.Phase) == ""
}

func cleanDiscovery(value Discovery) Discovery {
	value.Tool = strings.TrimSpace(value.Tool)
	value.Phase = strings.TrimSpace(value.Phase)
	return value
}
