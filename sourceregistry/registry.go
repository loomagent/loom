package sourceregistry

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const defaultConflictRetries = 3

type config struct {
	normalize       Normalizer
	conflictRetries int
}

// Option configures Registry.
type Option func(*config)

// WithNormalizer replaces the default conservative URL normalizer.
func WithNormalizer(normalizer Normalizer) Option {
	return func(cfg *config) {
		if normalizer != nil {
			cfg.normalize = normalizer
		}
	}
}

// WithConflictRetries sets the number of retries after Store returns
// ErrConflict. Zero disables retries.
func WithConflictRetries(retries int) Option {
	return func(cfg *config) {
		if retries >= 0 {
			cfg.conflictRetries = retries
		}
	}
}

// Registry binds a Store to one source-numbering namespace, normally a
// conversation ID. It is safe for concurrent use when its Store is safe for
// concurrent use, as required by the Store contract.
type Registry struct {
	namespace       string
	store           Store
	normalize       Normalizer
	conflictRetries int
}

// New constructs a registry for namespace.
func New(namespace string, store Store, options ...Option) (*Registry, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil, errors.New("sourceregistry: namespace is required")
	}
	if store == nil {
		return nil, errors.New("sourceregistry: Store is required")
	}
	cfg := config{normalize: NormalizeURL, conflictRetries: defaultConflictRetries}
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	return &Registry{namespace: namespace, store: store, normalize: cfg.normalize, conflictRetries: cfg.conflictRetries}, nil
}

// Namespace returns the source-numbering scope.
func (r *Registry) Namespace() string {
	if r == nil {
		return ""
	}
	return r.namespace
}

// Count returns the number of canonical sources in this namespace.
func (r *Registry) Count(ctx context.Context) (uint64, error) {
	if r == nil || r.store == nil {
		return 0, errors.New("sourceregistry: Registry is not initialized")
	}
	return r.store.Count(ctx, r.namespace)
}

// EnsureBatch atomically registers inputs. Blank URLs produce zero-value refs;
// a non-blank invalid URL aborts the batch before Store is called.
func (r *Registry) EnsureBatch(ctx context.Context, inputs []Input) ([]Ref, error) {
	if r == nil || r.store == nil || r.normalize == nil {
		return nil, errors.New("sourceregistry: Registry is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return []Ref{}, nil
	}

	keys := make([]string, len(inputs))
	unique := make([]Candidate, 0, len(inputs))
	indexByKey := make(map[string]int, len(inputs))
	for i, raw := range inputs {
		rawURL := strings.TrimSpace(raw.URL)
		if rawURL == "" {
			continue
		}
		key, err := r.normalize(rawURL)
		if err != nil {
			return nil, fmt.Errorf("sourceregistry: input %d: %w", i, err)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("sourceregistry: input %d: %w: normalizer returned an empty key", i, ErrInvalidURL)
		}
		keys[i] = key
		if strings.TrimSpace(raw.OriginalURL) == "" {
			raw.OriginalURL = rawURL
		}
		clean := cleanInput(raw, key)
		if index, exists := indexByKey[key]; exists {
			unique[index].Input = mergeInputs(unique[index].Input, clean)
			continue
		}
		indexByKey[key] = len(unique)
		unique = append(unique, Candidate{Key: key, Input: clean})
	}
	if len(unique) == 0 {
		return make([]Ref, len(inputs)), nil
	}

	var stored []StoredRef
	var err error
	for attempt := 0; attempt <= r.conflictRetries; attempt++ {
		stored, err = r.store.EnsureBatch(ctx, r.namespace, cloneCandidates(unique))
		if err == nil {
			break
		}
		if !errors.Is(err, ErrConflict) || attempt == r.conflictRetries {
			return nil, fmt.Errorf("sourceregistry: ensure batch: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if err := validateStored(unique, stored); err != nil {
		return nil, err
	}

	byKey := make(map[string]Ref, len(stored))
	for i, result := range stored {
		byKey[unique[i].Key] = Ref{
			Seq: result.Source.Seq, ID: SourceID(result.Source.Seq),
			RawPath: result.Source.RawPath, Created: result.Created,
		}
	}
	refs := make([]Ref, len(inputs))
	createdSeen := make(map[string]struct{}, len(stored))
	for i, key := range keys {
		if key != "" {
			refs[i] = byKey[key]
			if refs[i].Created {
				if _, seen := createdSeen[key]; seen {
					refs[i].Created = false
				} else {
					createdSeen[key] = struct{}{}
				}
			}
		}
	}
	return refs, nil
}

// SourceID formats a numeric sequence as its stable citation identifier.
func SourceID(seq uint64) string {
	if seq == 0 {
		return ""
	}
	return "SRC-" + strconv.FormatUint(seq, 10)
}

// RawPath returns the conventional workspace path for source content.
func RawPath(seq uint64) string {
	if seq == 0 {
		return ""
	}
	return "raw/" + SourceID(seq) + ".md"
}

func cleanInput(input Input, key string) Input {
	input.URL = key
	input.OriginalURL = strings.TrimSpace(input.OriginalURL)
	input.Origin = strings.TrimSpace(input.Origin)
	input.Title = strings.TrimSpace(input.Title)
	input.Snippet = strings.TrimSpace(input.Snippet)
	input.SearchDate = cleanDateEvidence(input.SearchDate)
	input.PublishedDate = clonePublishedDate(input.PublishedDate)
	input.Summary = strings.TrimSpace(input.Summary)
	input.Tier = strings.TrimSpace(input.Tier)
	input.Discovery = cleanDiscovery(input.Discovery)
	return input
}

func mergeInputs(firstInput, later Input) Input {
	firstInput.OriginalURL = first(firstInput.OriginalURL, later.OriginalURL)
	firstInput.Origin = first(firstInput.Origin, later.Origin)
	firstInput.Title = first(firstInput.Title, later.Title)
	firstInput.Snippet = first(firstInput.Snippet, later.Snippet)
	if firstInput.SearchDate.Text == "" && later.SearchDate.Text != "" {
		firstInput.SearchDate = later.SearchDate
	}
	if firstInput.PublishedDate.At == nil && firstInput.PublishedDate.Text == "" &&
		(later.PublishedDate.At != nil || later.PublishedDate.Text != "") {
		firstInput.PublishedDate = clonePublishedDate(later.PublishedDate)
	}
	firstInput.Summary = first(firstInput.Summary, later.Summary)
	firstInput.Tier = first(firstInput.Tier, later.Tier)
	if discoveryEmpty(firstInput.Discovery) && !discoveryEmpty(later.Discovery) {
		firstInput.Discovery = later.Discovery
	}
	firstInput.HasContent = firstInput.HasContent || later.HasContent
	return firstInput
}

func validateStored(candidates []Candidate, stored []StoredRef) error {
	if len(stored) != len(candidates) {
		return fmt.Errorf("%w: Store returned %d results for %d candidates", ErrInvalidStore, len(stored), len(candidates))
	}
	seenSeq := make(map[uint64]struct{}, len(stored))
	for i, result := range stored {
		if result.Source.Seq == 0 || result.Source.URL != candidates[i].Key {
			return fmt.Errorf("%w: result %d has seq=%d url=%q, expected url=%q", ErrInvalidStore, i, result.Source.Seq, result.Source.URL, candidates[i].Key)
		}
		if _, duplicate := seenSeq[result.Source.Seq]; duplicate {
			return fmt.Errorf("%w: duplicate sequence %d", ErrInvalidStore, result.Source.Seq)
		}
		seenSeq[result.Source.Seq] = struct{}{}
		if result.Source.HasContent && result.Source.RawPath != RawPath(result.Source.Seq) {
			return fmt.Errorf("%w: result %d has inconsistent raw path %q", ErrInvalidStore, i, result.Source.RawPath)
		}
		if !result.Source.HasContent && result.Source.RawPath != "" {
			return fmt.Errorf("%w: result %d has a raw path without content", ErrInvalidStore, i)
		}
	}
	return nil
}

func cloneCandidates(values []Candidate) []Candidate {
	out := make([]Candidate, len(values))
	copy(out, values)
	for i := range out {
		out[i].Input.PublishedDate = clonePublishedDate(out[i].Input.PublishedDate)
	}
	return out
}
