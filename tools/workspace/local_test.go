package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalBackendPersistsFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new-workspace")
	backend, err := OpenLocalBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	ctx := context.Background()
	if err := backend.Write(ctx, "/notes/report.md", "first\nsecond"); err != nil {
		t.Fatal(err)
	}
	got, err := backend.Read(ctx, "/notes/report.md", 2, 1)
	if err != nil || got != "second" {
		t.Fatalf("Read = %q, %v", got, err)
	}
	if _, err := backend.Edit(ctx, "/notes/report.md", "second", "updated", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "notes", "report.md"))
	if err != nil || string(data) != "first\nupdated" {
		t.Fatalf("disk content = %q, %v", data, err)
	}
}

func TestLocalBackendRequiresReadBeforeOverwrite(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	backend, err := OpenLocalBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	err = backend.Write(context.Background(), "/existing.md", "new")
	if !errors.Is(err, ErrMustReadFirst) {
		t.Fatalf("Write error = %v", err)
	}
}

func TestLocalBackendListsDirectoriesFirst(t *testing.T) {
	backend, err := OpenLocalBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	ctx := context.Background()
	if err := backend.Write(ctx, "/root.md", "root"); err != nil {
		t.Fatal(err)
	}
	if err := backend.Write(ctx, "/nested/file.md", "nested"); err != nil {
		t.Fatal(err)
	}
	entries, err := backend.Ls(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !entries[0].IsDir || entries[0].Path != "/nested" || entries[1].Path != "/root.md" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestLocalBackendRejectsTraversalAndEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	backend, err := OpenLocalBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if _, err := backend.Read(context.Background(), "/../secret", 0, 0); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("traversal error = %v", err)
	}
	if _, err := backend.Read(context.Background(), "/escape", 0, 0); err == nil {
		t.Fatal("escaping symlink read succeeded")
	}
}
