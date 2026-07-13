package sourceregistry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnsureBatchDeduplicatesPreservesOrderAndMerges(t *testing.T) {
	store := NewMemoryStore()
	registry := mustRegistry(t, "conversation-1", store)
	date := time.Date(2025, 5, 30, 0, 0, 0, 0, time.UTC)
	refs, err := registry.EnsureBatch(context.Background(), []Input{
		{URL: "https://EXAMPLE.com/a#top", Title: "First", SearchDate: DateEvidence{Text: "May 30", Source: "search"}},
		{URL: "", Title: "ignored"},
		{URL: "https://other.example/b", Title: "Second"},
		{URL: "https://example.com/a", Snippet: "merged", PublishedDate: PublishedDate{At: &date, Source: "reader"}, HasContent: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 4 || refs[0].Seq != 1 || refs[1].Seq != 0 || refs[2].Seq != 2 || refs[3].Seq != 1 {
		t.Fatalf("refs = %+v", refs)
	}
	if !refs[0].Created || refs[3].Created {
		t.Fatalf("Created double counted: %+v", refs)
	}
	if refs[0].RawPath != RawPath(1) || refs[3].RawPath != RawPath(1) {
		t.Fatalf("raw paths = %+v", refs)
	}
	sources, err := store.Sources(context.Background(), registry.Namespace())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].Title != "First" || sources[0].Snippet != "merged" || !sources[0].HasContent {
		t.Fatalf("sources = %+v", sources)
	}
	if sources[0].SearchDate.Source != "search" || sources[0].PublishedDate.Source != "reader" || sources[0].OriginalURL != "https://EXAMPLE.com/a#top" {
		t.Fatalf("source provenance = %+v", sources[0])
	}
}

func TestEnsureBatchUpgradesExistingWithoutReallocation(t *testing.T) {
	store := NewMemoryStore()
	registry := mustRegistry(t, "conversation", store)
	first, err := registry.EnsureBatch(context.Background(), []Input{{URL: "https://example.com/a", Title: "original"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.EnsureBatch(context.Background(), []Input{{URL: "https://example.com/a#fragment", Title: "replacement", Summary: "summary", HasContent: true}})
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Seq != second[0].Seq || second[0].Created || second[0].RawPath != "raw/SRC-1.md" {
		t.Fatalf("refs first=%+v second=%+v", first, second)
	}
	sources, _ := store.Sources(context.Background(), "conversation")
	if sources[0].Title != "original" || sources[0].Summary != "summary" {
		t.Fatalf("merge = %+v", sources[0])
	}
}

func TestMemoryStoreConcurrentAllocation(t *testing.T) {
	store := NewMemoryStore()
	registry := mustRegistry(t, "concurrent", store)
	const count = 100
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := registry.EnsureBatch(context.Background(), []Input{{URL: fmt.Sprintf("https://example.com/%d", i)}})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	sources, err := store.Sources(context.Background(), "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != count {
		t.Fatalf("sources=%d", len(sources))
	}
	for i, source := range sources {
		if source.Seq != uint64(i+1) {
			t.Fatalf("sequence gap at %d: %+v", i, source)
		}
	}
}

func TestMemoryStoreConcurrentDuplicate(t *testing.T) {
	store := NewMemoryStore()
	registry := mustRegistry(t, "same", store)
	const workers = 50
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refs, err := registry.EnsureBatch(context.Background(), []Input{{URL: "https://example.com/same"}})
			if err == nil && refs[0].Seq != 1 {
				err = fmt.Errorf("seq=%d", refs[0].Seq)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count, err := registry.Count(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestMemoryStoreReturnsDeepCopies(t *testing.T) {
	store := NewMemoryStore()
	registry := mustRegistry(t, "copy", store)
	at := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	_, err := registry.EnsureBatch(context.Background(), []Input{{URL: "https://example.com", PublishedDate: PublishedDate{At: &at}}})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.Sources(context.Background(), "copy")
	first[0].PublishedDate.At = nil
	second, _ := store.Sources(context.Background(), "copy")
	if second[0].PublishedDate.At == nil {
		t.Fatal("stored pointer aliased returned snapshot")
	}
}

func TestCanceledContextDoesNotMutate(t *testing.T) {
	store := NewMemoryStore()
	registry := mustRegistry(t, "cancel", store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.EnsureBatch(ctx, []Input{{URL: "https://example.com"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	count, _ := registry.Count(context.Background())
	if count != 0 {
		t.Fatalf("count=%d", count)
	}
}

func TestNamespacesHaveIndependentSequences(t *testing.T) {
	store := NewMemoryStore()
	a := mustRegistry(t, "a", store)
	b := mustRegistry(t, "b", store)
	ra, _ := a.EnsureBatch(context.Background(), []Input{{URL: "https://a.example"}})
	rb, _ := b.EnsureBatch(context.Background(), []Input{{URL: "https://b.example"}})
	if ra[0].Seq != 1 || rb[0].Seq != 1 {
		t.Fatalf("a=%+v b=%+v", ra, rb)
	}
}

func TestInvalidBatchHasNoEffects(t *testing.T) {
	store := NewMemoryStore()
	registry := mustRegistry(t, "atomic", store)
	_, err := registry.EnsureBatch(context.Background(), []Input{{URL: "https://ok.example"}, {URL: "relative"}})
	if !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("error=%v", err)
	}
	count, _ := registry.Count(context.Background())
	if count != 0 {
		t.Fatalf("count=%d", count)
	}
	_, err = store.EnsureBatch(context.Background(), "atomic", []Candidate{{Key: "a", Input: Input{URL: "a"}}, {Key: "", Input: Input{}}})
	if !errors.Is(err, ErrInvalidStore) {
		t.Fatalf("store error=%v", err)
	}
	count, _ = registry.Count(context.Background())
	if count != 0 {
		t.Fatalf("store partially committed, count=%d", count)
	}
}

type conflictStore struct {
	calls    atomic.Int32
	delegate *MemoryStore
}

func (s *conflictStore) EnsureBatch(ctx context.Context, namespace string, values []Candidate) ([]StoredRef, error) {
	if s.calls.Add(1) < 3 {
		return nil, ErrConflict
	}
	return s.delegate.EnsureBatch(ctx, namespace, values)
}
func (s *conflictStore) Count(ctx context.Context, namespace string) (uint64, error) {
	return s.delegate.Count(ctx, namespace)
}

func TestRegistryRetriesUncommittedConflicts(t *testing.T) {
	store := &conflictStore{delegate: NewMemoryStore()}
	registry := mustRegistry(t, "retry", store)
	refs, err := registry.EnsureBatch(context.Background(), []Input{{URL: "https://example.com"}})
	if err != nil || refs[0].Seq != 1 || store.calls.Load() != 3 {
		t.Fatalf("refs=%+v calls=%d err=%v", refs, store.calls.Load(), err)
	}
}

func TestContextHelpers(t *testing.T) {
	registry := mustRegistry(t, "ctx", NewMemoryStore())
	ctx := WithContext(context.Background(), registry)
	if FromContext(ctx) != registry || FromContext(context.Background()) != nil {
		t.Fatal("context registry mismatch")
	}
}

func mustRegistry(t *testing.T, namespace string, store Store) *Registry {
	t.Helper()
	registry, err := New(namespace, store)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
