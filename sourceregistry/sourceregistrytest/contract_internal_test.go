package sourceregistrytest

import (
	"testing"
	"time"

	"github.com/loomagent/loom/sourceregistry"
)

// The suite hands candidates to a Store, so the copy it passes must not share the
// caller's evidence: a Store that writes through the pointer would otherwise mutate the
// test's own fixtures and hide the damage.
func TestCloneCandidatesCopiesTheEvidence(t *testing.T) {
	at := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	original := []sourceregistry.Candidate{{
		Key:   "https://contract.example/a",
		Input: sourceregistry.Input{Title: "a", PublishedDate: sourceregistry.PublishedDate{At: &at, Text: "17 Aug 2026"}},
	}}

	cloned := cloneCandidates(original)
	if len(cloned) != 1 || cloned[0].Key != original[0].Key {
		t.Fatalf("clone = %+v", cloned)
	}
	if cloned[0].Input.PublishedDate.At == original[0].Input.PublishedDate.At {
		t.Fatal("the clone shares the date pointer")
	}
	if !cloned[0].Input.PublishedDate.At.Equal(at) || cloned[0].Input.PublishedDate.Text != "17 Aug 2026" {
		t.Fatalf("clone evidence = %+v", cloned[0].Input.PublishedDate)
	}

	// Writing through the clone leaves the original alone.
	*cloned[0].Input.PublishedDate.At = at.AddDate(1, 0, 0)
	cloned[0].Input.Title = "changed"
	if !original[0].Input.PublishedDate.At.Equal(at) || original[0].Input.Title != "a" {
		t.Fatalf("the original changed: %+v", original[0])
	}

	// A candidate without a date clones without one.
	plain := cloneCandidates([]sourceregistry.Candidate{{Key: "k"}})
	if plain[0].Input.PublishedDate.At != nil {
		t.Fatalf("plain clone = %+v", plain[0])
	}
}
