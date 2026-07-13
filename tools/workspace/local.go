package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
)

// LocalBackend stores workspace files beneath a local directory. Filesystem
// access is confined by os.Root, including when workspace files contain
// symbolic links.
type LocalBackend struct {
	mu        sync.Mutex
	root      *os.Root
	readPaths map[string]struct{}
}

// OpenLocalBackend opens root as a local workspace, creating the directory when
// necessary. Call Close when the backend is no longer needed.
func OpenLocalBackend(root string) (*LocalBackend, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("workspace: local root is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("workspace: create local root: %w", err)
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: open local root: %w", err)
	}
	return &LocalBackend{root: confined, readPaths: map[string]struct{}{}}, nil
}

// Close releases the filesystem root held by the backend.
func (b *LocalBackend) Close() error {
	if b == nil || b.root == nil {
		return nil
	}
	return b.root.Close()
}

// Ls lists direct children of a directory.
func (b *LocalBackend) Ls(_ context.Context, dir string) ([]FileInfo, error) {
	dir, err := ValidateDirPath(dir)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	entries, err := fs.ReadDir(b.root.FS(), localName(dir))
	if err != nil {
		if errorsIsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrFileNotFound, dir)
		}
		return nil, fmt.Errorf("workspace: list %s: %w", dir, err)
	}
	out := make([]FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("workspace: stat %s: %w", entry.Name(), err)
		}
		out = append(out, FileInfo{
			Path:       path.Join(dir, entry.Name()),
			IsDir:      entry.IsDir(),
			Size:       info.Size(),
			ModifiedAt: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Read reads a file and marks it as eligible for subsequent writes and edits.
func (b *LocalBackend) Read(_ context.Context, workspacePath string, offset, limit int) (string, error) {
	workspacePath, err := ValidatePath(workspacePath)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	data, err := b.root.ReadFile(localName(workspacePath))
	if err != nil {
		if errorsIsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrFileNotFound, workspacePath)
		}
		return "", fmt.Errorf("workspace: read %s: %w", workspacePath, err)
	}
	if len(data) > MaxFileBytes {
		return "", fmt.Errorf("%w: %d bytes (limit %d)", ErrFileTooLarge, len(data), MaxFileBytes)
	}
	b.readPaths[workspacePath] = struct{}{}
	return sliceLines(string(data), offset, limit), nil
}

// Write creates or replaces a file. Replacing an existing file requires a
// prior Read on the same backend instance.
func (b *LocalBackend) Write(_ context.Context, workspacePath, content string) error {
	workspacePath, err := ValidatePath(workspacePath)
	if err != nil {
		return err
	}
	if len(content) > MaxFileBytes {
		return fmt.Errorf("%w: %d bytes (limit %d)", ErrFileTooLarge, len(content), MaxFileBytes)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	name := localName(workspacePath)
	_, statErr := b.root.Stat(name)
	exists := statErr == nil
	if statErr != nil && !errorsIsNotExist(statErr) {
		return fmt.Errorf("workspace: stat %s: %w", workspacePath, statErr)
	}
	if exists {
		if _, read := b.readPaths[workspacePath]; !read {
			return fmt.Errorf("%w: %s (call read_file first)", ErrMustReadFirst, workspacePath)
		}
	} else {
		count, err := b.fileCount()
		if err != nil {
			return err
		}
		if count >= MaxWorkspaceFiles {
			return fmt.Errorf("%w: %d (limit %d)", ErrTooManyFiles, count, MaxWorkspaceFiles)
		}
	}
	if parent := path.Dir(name); parent != "." {
		if err := b.root.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("workspace: create parent for %s: %w", workspacePath, err)
		}
	}
	if err := b.root.WriteFile(name, []byte(content), 0o644); err != nil {
		return fmt.Errorf("workspace: write %s: %w", workspacePath, err)
	}
	b.readPaths[workspacePath] = struct{}{}
	return nil
}

// Edit replaces matching text in an existing, previously read file.
func (b *LocalBackend) Edit(_ context.Context, workspacePath, oldString, newString string, replaceAll bool) (int, error) {
	workspacePath, err := ValidatePath(workspacePath)
	if err != nil {
		return 0, err
	}
	if oldString == "" {
		return 0, ErrEmptyOldString
	}
	if oldString == newString {
		return 0, ErrSameOldAndNew
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, read := b.readPaths[workspacePath]; !read {
		if _, err := b.root.Stat(localName(workspacePath)); err != nil && errorsIsNotExist(err) {
			return 0, fmt.Errorf("%w: %s", ErrFileNotFound, workspacePath)
		}
		return 0, fmt.Errorf("%w: %s (call read_file first)", ErrMustReadFirst, workspacePath)
	}
	data, err := b.root.ReadFile(localName(workspacePath))
	if err != nil {
		if errorsIsNotExist(err) {
			return 0, fmt.Errorf("%w: %s", ErrFileNotFound, workspacePath)
		}
		return 0, fmt.Errorf("workspace: read %s: %w", workspacePath, err)
	}
	count := strings.Count(string(data), oldString)
	if count == 0 {
		return 0, ErrEditNoMatch
	}
	if count > 1 && !replaceAll {
		return 0, fmt.Errorf("%w: %d occurrences", ErrEditAmbiguous, count)
	}
	newContent := strings.Replace(string(data), oldString, newString, 1)
	if replaceAll {
		newContent = strings.ReplaceAll(string(data), oldString, newString)
	} else {
		count = 1
	}
	if len(newContent) > MaxFileBytes {
		return 0, fmt.Errorf("%w: %d bytes after edit (limit %d)", ErrFileTooLarge, len(newContent), MaxFileBytes)
	}
	if err := b.root.WriteFile(localName(workspacePath), []byte(newContent), 0o644); err != nil {
		return 0, fmt.Errorf("workspace: write %s: %w", workspacePath, err)
	}
	return count, nil
}

func (b *LocalBackend) fileCount() (int, error) {
	count := 0
	err := fs.WalkDir(b.root.FS(), ".", func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("workspace: count local files: %w", err)
	}
	return count, nil
}

func localName(workspacePath string) string {
	name := strings.TrimPrefix(workspacePath, "/")
	if name == "" {
		return "."
	}
	return name
}

func errorsIsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

var _ Backend = (*LocalBackend)(nil)
