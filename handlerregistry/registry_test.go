package handlerregistry

import (
	"context"
	"testing"

	"github.com/loomagent/loom"
)

func stubHandler(context.Context, loom.TurnWriter, []loom.Turn, loom.UserMessage) error {
	return nil
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register("assistant", stubHandler)

	h, ok := r.Lookup("assistant")
	if !ok || h == nil {
		t.Fatal("Lookup() did not return registered handler")
	}
	if _, ok := r.Lookup("missing"); ok {
		t.Fatal("Lookup() found unregistered handler")
	}
	keys := r.Keys()
	if len(keys) != 1 || keys[0] != "assistant" {
		t.Fatalf("Keys() = %v", keys)
	}
}

func TestRegistryRejectsDuplicate(t *testing.T) {
	r := NewRegistry()
	r.Register("assistant", stubHandler)
	mustPanic(t, func() { r.Register("assistant", stubHandler) })
}

func TestRegistryRejectsNilHandler(t *testing.T) {
	mustPanic(t, func() { NewRegistry().Register("assistant", nil) })
}

func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}
