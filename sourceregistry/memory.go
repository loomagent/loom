package sourceregistry

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// MemoryStore is a concurrency-safe Store for tests, local agents, and
// ephemeral runs. Numbering is independent per namespace.
type MemoryStore struct {
	collections sync.Map // map[string]*memoryCollection
}

type memoryCollection struct {
	mu      sync.Mutex
	nextSeq uint64
	byURL   map[string]Source
}

// NewMemoryStore constructs an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (s *MemoryStore) collection(namespace string) *memoryCollection {
	value, _ := s.collections.LoadOrStore(namespace, &memoryCollection{nextSeq: 1, byURL: map[string]Source{}})
	return value.(*memoryCollection)
}

// EnsureBatch implements Store.
func (s *MemoryStore) EnsureBatch(ctx context.Context, namespace string, candidates []Candidate) ([]StoredRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	collection := s.collection(namespace)
	collection.mu.Lock()
	defer collection.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(candidates))
	for i, candidate := range candidates {
		if candidate.Key == "" || candidate.Input.URL != candidate.Key {
			return nil, fmt.Errorf("%w: candidate %d has inconsistent key", ErrInvalidStore, i)
		}
		if _, duplicate := seen[candidate.Key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate candidate key %q", ErrInvalidStore, candidate.Key)
		}
		seen[candidate.Key] = struct{}{}
	}

	results := make([]StoredRef, len(candidates))
	for i, candidate := range candidates {
		source, exists := collection.byURL[candidate.Key]
		created := !exists
		if created {
			source = NewSource(collection.nextSeq, candidate)
			collection.nextSeq++
		} else {
			source, _ = MergeSource(source, candidate)
		}
		collection.byURL[candidate.Key] = cloneSource(source)
		results[i] = StoredRef{Source: cloneSource(source), Created: created}
	}
	return results, nil
}

// Count implements Store.
func (s *MemoryStore) Count(ctx context.Context, namespace string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	collection := s.collection(namespace)
	collection.mu.Lock()
	defer collection.mu.Unlock()
	return uint64(len(collection.byURL)), nil
}

// Sources returns a sequence-sorted snapshot for inspection and persistence in
// tests or small local applications.
func (s *MemoryStore) Sources(ctx context.Context, namespace string) ([]Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	collection := s.collection(namespace)
	collection.mu.Lock()
	defer collection.mu.Unlock()
	out := make([]Source, 0, len(collection.byURL))
	for _, source := range collection.byURL {
		out = append(out, cloneSource(source))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func cloneSource(source Source) Source {
	source.PublishedDate = clonePublishedDate(source.PublishedDate)
	return source
}

var _ Store = (*MemoryStore)(nil)
