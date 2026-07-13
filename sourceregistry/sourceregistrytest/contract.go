// Package sourceregistrytest provides a reusable conformance suite for
// sourceregistry.Store implementations.
package sourceregistrytest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loomagent/loom/sourceregistry"
)

// Factory returns a fresh Store for one subtest. Implementations may register
// cleanup through t.Cleanup. Each call must return an independent logical
// store; sharing an external database is fine because the suite also uses
// unique namespaces.
type Factory func(t *testing.T) sourceregistry.Store

var namespaceSequence atomic.Uint64

// TestStore runs the complete Store contract. Call it from an implementation's
// own test package:
//
//	func TestStoreContract(t *testing.T) {
//		sourceregistrytest.TestStore(t, func(t *testing.T) sourceregistry.Store {
//			return newTestStore(t)
//		})
//	}
func TestStore(t *testing.T, factory Factory) {
	t.Helper()
	if factory == nil {
		t.Fatal("sourceregistrytest: Factory is required")
	}

	t.Run("empty", func(t *testing.T) {
		store := requireStore(t, factory)
		namespace := uniqueNamespace(t)
		results, err := store.EnsureBatch(context.Background(), namespace, []sourceregistry.Candidate{})
		if err != nil {
			t.Fatalf("EnsureBatch(empty): %v", err)
		}
		if results == nil || len(results) != 0 {
			t.Fatalf("EnsureBatch(empty) = %#v, want non-nil empty slice", results)
		}
		if count := countSources(t, store, namespace); count != 0 {
			t.Fatalf("Count = %d, want 0", count)
		}
	})

	t.Run("batch_order_and_contiguous_sequences", func(t *testing.T) {
		store := requireStore(t, factory)
		namespace := uniqueNamespace(t)
		candidates := candidates("a", "b", "c")
		original := cloneCandidates(candidates)
		results := ensure(t, store, namespace, candidates)
		if !reflect.DeepEqual(candidates, original) {
			t.Fatalf("Store mutated candidates:\n got=%+v\nwant=%+v", candidates, original)
		}
		for i, result := range results {
			wantSeq := uint64(i + 1)
			if result.Source.Seq != wantSeq || result.Source.URL != candidates[i].Key || !result.Created {
				t.Fatalf("result[%d] = %+v, want seq=%d key=%q created", i, result, wantSeq, candidates[i].Key)
			}
		}
		if count := countSources(t, store, namespace); count != 3 {
			t.Fatalf("Count = %d, want 3", count)
		}
	})

	t.Run("deduplicate_and_merge", func(t *testing.T) {
		store := requireStore(t, factory)
		namespace := uniqueNamespace(t)
		first := candidate("same", sourceregistry.Input{Title: "original", SearchDate: sourceregistry.DateEvidence{Text: "old", Source: "first"}})
		created := ensure(t, store, namespace, []sourceregistry.Candidate{first})[0]
		upgrade := candidate("same", sourceregistry.Input{Title: "replacement", Summary: "summary", HasContent: true})
		updated := ensure(t, store, namespace, []sourceregistry.Candidate{upgrade})[0]
		if created.Source.Seq != updated.Source.Seq || updated.Created {
			t.Fatalf("created=%+v updated=%+v", created, updated)
		}
		if updated.Source.Title != "original" || updated.Source.Summary != "summary" || !updated.Source.HasContent {
			t.Fatalf("merged source = %+v", updated.Source)
		}
		if updated.Source.RawPath != sourceregistry.RawPath(updated.Source.Seq) {
			t.Fatalf("RawPath = %q", updated.Source.RawPath)
		}
		if count := countSources(t, store, namespace); count != 1 {
			t.Fatalf("Count = %d, want 1", count)
		}
	})

	t.Run("namespace_isolation", func(t *testing.T) {
		store := requireStore(t, factory)
		left, right := uniqueNamespace(t)+"/left", uniqueNamespace(t)+"/right"
		leftResult := ensure(t, store, left, candidates("source"))[0]
		rightResult := ensure(t, store, right, candidates("source"))[0]
		if leftResult.Source.Seq != 1 || rightResult.Source.Seq != 1 || !leftResult.Created || !rightResult.Created {
			t.Fatalf("left=%+v right=%+v", leftResult, rightResult)
		}
	})

	t.Run("returned_values_do_not_alias_storage", func(t *testing.T) {
		store := requireStore(t, factory)
		namespace := uniqueNamespace(t)
		at := time.Date(2025, 5, 30, 0, 0, 0, 0, time.UTC)
		item := candidate("copy", sourceregistry.Input{PublishedDate: sourceregistry.PublishedDate{At: &at, Source: "reader"}})
		first := ensure(t, store, namespace, []sourceregistry.Candidate{item})
		first[0].Source.PublishedDate.At = nil
		second := ensure(t, store, namespace, []sourceregistry.Candidate{item})
		if second[0].Source.PublishedDate.At == nil {
			t.Fatal("mutating returned Source changed stored state")
		}
	})

	t.Run("canceled_context_has_no_effect", func(t *testing.T) {
		store := requireStore(t, factory)
		namespace := uniqueNamespace(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := store.EnsureBatch(ctx, namespace, candidates("canceled"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("EnsureBatch error = %v, want context.Canceled", err)
		}
		if count := countSources(t, store, namespace); count != 0 {
			t.Fatalf("canceled call committed %d sources", count)
		}
	})

	t.Run("concurrent_disjoint_batches", func(t *testing.T) {
		store := requireStore(t, factory)
		namespace := uniqueNamespace(t)
		registry := requireRegistry(t, namespace, store)
		const workers = 32
		var wg sync.WaitGroup
		seqs := make(chan uint64, workers)
		errs := make(chan error, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				refs, err := registry.EnsureBatch(context.Background(), []sourceregistry.Input{{URL: fmt.Sprintf("https://contract.example/%d", i)}})
				if err == nil {
					seqs <- refs[0].Seq
				}
				errs <- err
			}(i)
		}
		wg.Wait()
		close(seqs)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent EnsureBatch: %v", err)
			}
		}
		got := make([]uint64, 0, workers)
		for seq := range seqs {
			got = append(got, seq)
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		for i, seq := range got {
			if seq != uint64(i+1) {
				t.Fatalf("sequences = %v, want contiguous 1..%d", got, workers)
			}
		}
	})

	t.Run("concurrent_same_source", func(t *testing.T) {
		store := requireStore(t, factory)
		namespace := uniqueNamespace(t)
		registry := requireRegistry(t, namespace, store)
		const workers = 32
		var wg sync.WaitGroup
		refs := make(chan sourceregistry.Ref, workers)
		errs := make(chan error, workers)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := registry.EnsureBatch(context.Background(), []sourceregistry.Input{{URL: "https://contract.example/same"}})
				if err == nil {
					refs <- got[0]
				}
				errs <- err
			}()
		}
		wg.Wait()
		close(refs)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent EnsureBatch: %v", err)
			}
		}
		created := 0
		for ref := range refs {
			if ref.Seq != 1 {
				t.Fatalf("Ref.Seq = %d, want 1", ref.Seq)
			}
			if ref.Created {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("Created count = %d, want 1", created)
		}
		if count := countSources(t, store, namespace); count != 1 {
			t.Fatalf("Count = %d, want 1", count)
		}
	})
}

func requireStore(t *testing.T, factory Factory) sourceregistry.Store {
	t.Helper()
	store := factory(t)
	if store == nil {
		t.Fatal("sourceregistrytest: Factory returned nil Store")
	}
	return store
}

func requireRegistry(t *testing.T, namespace string, store sourceregistry.Store) *sourceregistry.Registry {
	t.Helper()
	registry, err := sourceregistry.New(namespace, store, sourceregistry.WithConflictRetries(10))
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func uniqueNamespace(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("loom-store-contract/%d/%d/%s", time.Now().UnixNano(), namespaceSequence.Add(1), t.Name())
}

func candidate(name string, input sourceregistry.Input) sourceregistry.Candidate {
	key := "https://contract.example/" + name
	input.URL = key
	return sourceregistry.Candidate{Key: key, Input: input}
}

func candidates(names ...string) []sourceregistry.Candidate {
	out := make([]sourceregistry.Candidate, len(names))
	for i, name := range names {
		out[i] = candidate(name, sourceregistry.Input{Title: name})
	}
	return out
}

func cloneCandidates(values []sourceregistry.Candidate) []sourceregistry.Candidate {
	out := make([]sourceregistry.Candidate, len(values))
	copy(out, values)
	for i := range out {
		if at := out[i].Input.PublishedDate.At; at != nil {
			copyAt := *at
			out[i].Input.PublishedDate.At = &copyAt
		}
	}
	return out
}

func ensure(t *testing.T, store sourceregistry.Store, namespace string, values []sourceregistry.Candidate) []sourceregistry.StoredRef {
	t.Helper()
	results, err := store.EnsureBatch(context.Background(), namespace, values)
	if err != nil {
		t.Fatalf("EnsureBatch: %v", err)
	}
	if len(results) != len(values) {
		t.Fatalf("EnsureBatch returned %d results for %d candidates", len(results), len(values))
	}
	return results
}

func countSources(t *testing.T, store sourceregistry.Store, namespace string) uint64 {
	t.Helper()
	count, err := store.Count(context.Background(), namespace)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	return count
}
