package sourceregistry

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	if first[0].Seq != second[0].Seq || second[0].Created {
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
	for i := range count {
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
	for range workers {
		wg.Go(func() {
			refs, err := registry.EnsureBatch(context.Background(), []Input{{URL: "https://example.com/same"}})
			if err == nil && refs[0].Seq != 1 {
				err = fmt.Errorf("seq=%d", refs[0].Seq)
			}
			errs <- err
		})
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

// countingStore delegates to a real store and can refuse the first attempts, which is how a
// concurrent registration looks from the losing side.
type countingStore struct {
	delegate  Store
	conflicts int
	calls     int
}

func (s *countingStore) EnsureBatch(ctx context.Context, namespace string, candidates []Candidate) ([]StoredRef, error) {
	s.calls++
	if s.calls <= s.conflicts {
		return nil, ErrConflict
	}
	return s.delegate.EnsureBatch(ctx, namespace, candidates)
}

func (s *countingStore) Count(ctx context.Context, namespace string) (uint64, error) {
	return s.delegate.Count(ctx, namespace)
}

// The conflict budget is a knob, so both ends of it have to behave: no retries fails
// immediately, and more retries get through more conflicts.
func TestWithConflictRetriesControlsTheAttempts(t *testing.T) {
	for _, testCase := range []struct {
		retries   int
		conflicts int
		wantErr   bool
		wantCalls int
	}{
		{retries: 0, conflicts: 1, wantErr: true, wantCalls: 1},
		{retries: 2, conflicts: 2, wantCalls: 3},
		{retries: 2, conflicts: 3, wantErr: true, wantCalls: 3},
		{retries: 4, conflicts: 4, wantCalls: 5},
	} {
		store := &countingStore{delegate: NewMemoryStore(), conflicts: testCase.conflicts}
		registry, err := New("retries", store, WithConflictRetries(testCase.retries))
		if err != nil {
			t.Fatal(err)
		}
		refs, err := registry.EnsureBatch(context.Background(), []Input{{URL: "https://example.com"}})
		if testCase.wantErr {
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("retries=%d conflicts=%d err=%v", testCase.retries, testCase.conflicts, err)
			}
		} else if err != nil || len(refs) != 1 || refs[0].Seq != 1 {
			t.Fatalf("retries=%d conflicts=%d refs=%+v err=%v", testCase.retries, testCase.conflicts, refs, err)
		}
		if store.calls != testCase.wantCalls {
			t.Errorf("retries=%d conflicts=%d calls=%d, want %d", testCase.retries, testCase.conflicts, store.calls, testCase.wantCalls)
		}
	}
}

// A caller with its own idea of a canonical URL replaces the conservative default, and two
// spellings it considers equal then share one reference.
func TestWithNormalizerReplacesTheKey(t *testing.T) {
	stripTracking := func(raw string) (string, error) {
		if before, _, found := strings.Cut(raw, "?"); found {
			return before, nil
		}
		return raw, nil
	}
	registry, err := New("custom", NewMemoryStore(), WithNormalizer(stripTracking))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := registry.EnsureBatch(context.Background(), []Input{
		{URL: "https://example.com/report?utm_source=a"},
		{URL: "https://example.com/report?utm_source=b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].Seq != refs[1].Seq {
		t.Fatalf("refs = %+v", refs)
	}

	// A nil normalizer leaves the default in place rather than removing deduplication.
	registry, err = New("nil-normalizer", NewMemoryStore(), WithNormalizer(nil))
	if err != nil {
		t.Fatal(err)
	}
	refs, err = registry.EnsureBatch(context.Background(), []Input{
		{URL: "https://example.com/report?a=1"},
		{URL: "https://example.com/report?a=1"},
	})
	if err != nil || refs[0].Seq != refs[1].Seq {
		t.Fatalf("refs = %+v err = %v", refs, err)
	}
}

// A normalizer is caller-supplied, so its failures are reported with the input that caused
// them rather than as a broken batch.
func TestNormalizerFailuresAreReportedPerInput(t *testing.T) {
	failing := func(string) (string, error) { return "", errors.New("no key for you") }
	registry, err := New("failing", NewMemoryStore(), WithNormalizer(failing))
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.EnsureBatch(context.Background(), []Input{{URL: "https://example.com"}, {URL: "https://example.org"}})
	if err == nil || !strings.Contains(err.Error(), "input 0: no key for you") {
		t.Fatalf("error = %v", err)
	}

	empty := func(string) (string, error) { return "   ", nil }
	registry, err = New("empty", NewMemoryStore(), WithNormalizer(empty))
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.EnsureBatch(context.Background(), []Input{{URL: "https://example.com"}})
	if !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewValidatesItsArguments(t *testing.T) {
	if _, err := New("", NewMemoryStore()); err == nil || !strings.Contains(err.Error(), "namespace is required") {
		t.Fatalf("error = %v", err)
	}
	if _, err := New("   ", NewMemoryStore()); err == nil {
		t.Fatal("a blank namespace must be refused")
	}
	if _, err := New("ns", nil); err == nil || !strings.Contains(err.Error(), "Store is required") {
		t.Fatalf("error = %v", err)
	}
	// A nil option is skipped, and the namespace is trimmed.
	registry, err := New("  spaced  ", NewMemoryStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if registry.Namespace() != "spaced" {
		t.Fatalf("Namespace() = %q", registry.Namespace())
	}
}

// Accessors on an uninitialized registry report rather than panic, because a nil registry
// is what a caller has before it constructs one.
func TestUninitializedRegistry(t *testing.T) {
	var registry *Registry
	if registry.Namespace() != "" {
		t.Fatalf("Namespace() = %q", registry.Namespace())
	}
	if _, err := registry.Count(context.Background()); err == nil {
		t.Fatal("Count must report an uninitialized registry")
	}
	if _, err := registry.EnsureBatch(context.Background(), []Input{{URL: "https://example.com"}}); err == nil {
		t.Fatal("EnsureBatch must report an uninitialized registry")
	}
}

// Blank inputs carry no source, so they come back as empty references instead of failing
// the batch or being sent to the Store.
func TestEnsureBatchSkipsBlankInputs(t *testing.T) {
	store := &countingStore{delegate: NewMemoryStore()}
	registry, err := New("blank", store)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := registry.EnsureBatch(context.Background(), []Input{{URL: "   "}, {URL: "https://example.com"}, {URL: ""}})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || refs[0] != (Ref{}) || refs[2] != (Ref{}) || refs[1].Seq != 1 {
		t.Fatalf("refs = %+v", refs)
	}

	if refs, err := registry.EnsureBatch(context.Background(), []Input{{URL: ""}}); err != nil || len(refs) != 1 || refs[0] != (Ref{}) {
		t.Fatalf("refs = %+v err = %v", refs, err)
	}
	if refs, err := registry.EnsureBatch(context.Background(), nil); err != nil || len(refs) != 0 {
		t.Fatalf("refs = %+v err = %v", refs, err)
	}
}

// A cancelation that lands while the registry is backing off from a conflict ends the
// batch with the context's error rather than another attempt.
func TestConflictRetryStopsOnCancelation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &cancelOnConflictStore{delegate: NewMemoryStore(), cancel: cancel}
	registry, err := New("cancel", store, WithConflictRetries(5))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.EnsureBatch(ctx, []Input{{URL: "https://example.com"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("calls = %d, want the batch to stop after the first conflict", store.calls)
	}
}

type cancelOnConflictStore struct {
	delegate Store
	cancel   context.CancelFunc
	calls    int
}

func (s *cancelOnConflictStore) EnsureBatch(ctx context.Context, namespace string, candidates []Candidate) ([]StoredRef, error) {
	s.calls++
	// The cancelation arrives from outside while this attempt is failing.
	s.cancel()
	return nil, ErrConflict
}

func (s *cancelOnConflictStore) Count(ctx context.Context, namespace string) (uint64, error) {
	return s.delegate.Count(ctx, namespace)
}
