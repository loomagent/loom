// Package sourceregistry assigns stable, conversation-scoped references to
// research sources without prescribing a database or search provider.
package sourceregistry

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidURL   = errors.New("sourceregistry: invalid URL")
	ErrInvalidStore = errors.New("sourceregistry: invalid store result")
	ErrConflict     = errors.New("sourceregistry: concurrent store conflict")
)

// DateEvidence keeps a provider date hint and its provenance together.
type DateEvidence struct {
	Text   string `json:"text,omitempty"`
	Source string `json:"source,omitempty"`
}

// PublishedDate contains publication-date evidence extracted from a source.
type PublishedDate struct {
	At         *time.Time `json:"at,omitempty"`
	Text       string     `json:"text,omitempty"`
	Source     string     `json:"source,omitempty"`
	Confidence string     `json:"confidence,omitempty"`
}

// Discovery records where a source was first encountered.
type Discovery struct {
	TurnIndex uint64 `json:"turn_index,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Round     uint64 `json:"round,omitempty"`
}

// Input is one source observation. Origin is application-defined; common
// values include "web_search", "web_reader", and "import".
type Input struct {
	URL           string        `json:"url"`
	OriginalURL   string        `json:"original_url,omitempty"`
	Origin        string        `json:"origin,omitempty"`
	Title         string        `json:"title,omitempty"`
	Snippet       string        `json:"snippet,omitempty"`
	SearchDate    DateEvidence  `json:"search_date,omitempty"`
	PublishedDate PublishedDate `json:"published_date,omitempty"`
	Summary       string        `json:"summary,omitempty"`
	Tier          string        `json:"tier,omitempty"`
	Discovery     Discovery     `json:"discovery,omitempty"`
	HasContent    bool          `json:"has_content,omitempty"`
}

// Source is the storage-neutral canonical record.
type Source struct {
	Seq           uint64        `json:"seq"`
	URL           string        `json:"url"`
	OriginalURL   string        `json:"original_url,omitempty"`
	Domain        string        `json:"domain,omitempty"`
	Origin        string        `json:"origin,omitempty"`
	Title         string        `json:"title,omitempty"`
	Snippet       string        `json:"snippet,omitempty"`
	SearchDate    DateEvidence  `json:"search_date,omitempty"`
	PublishedDate PublishedDate `json:"published_date,omitempty"`
	Summary       string        `json:"summary,omitempty"`
	Tier          string        `json:"tier,omitempty"`
	Discovery     Discovery     `json:"discovery,omitempty"`
	HasContent    bool          `json:"has_content,omitempty"`
	RawPath       string        `json:"raw_path,omitempty"`
}

// Candidate is a normalized, unique input passed to Store. Stores may assume
// Key equals Input.URL and that candidates appear in allocation order.
type Candidate struct {
	Key   string `json:"key"`
	Input Input  `json:"input"`
}

// StoredRef is returned by Store for one Candidate.
type StoredRef struct {
	Source  Source `json:"source"`
	Created bool   `json:"created"`
}

// Ref is returned to callers one-for-one with their original inputs. Duplicate
// inputs share Seq, ID, and RawPath. Created is true only on the first input
// that created the canonical source, so counting Created never double-counts.
type Ref struct {
	Seq     uint64 `json:"seq,omitempty"`
	ID      string `json:"id,omitempty"`
	RawPath string `json:"raw_path,omitempty"`
	Created bool   `json:"created,omitempty"`
}

// Store atomically registers a normalized batch inside one namespace.
//
// EnsureBatch must be linearizable per namespace, including across processes,
// and enforce uniqueness of both (namespace, Candidate.Key) and
// (namespace, Source.Seq). It must return one result per candidate in the same order. It must
// deduplicate against existing records, allocate consecutive increasing Seq
// values to new candidates in input order, apply MergeSource to existing
// records, and commit all changes or none. Cross-process implementations may
// return ErrConflict only when no effects were committed; Registry will retry
// the complete batch. Inputs must not be mutated.
type Store interface {
	EnsureBatch(ctx context.Context, namespace string, candidates []Candidate) ([]StoredRef, error)
	Count(ctx context.Context, namespace string) (uint64, error)
}

// Normalizer converts a caller URL into a non-empty, trimmed deduplication key.
// It is not called for blank input.
type Normalizer func(string) (string, error)
