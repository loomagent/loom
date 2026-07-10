package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInMemory_WriteReadBasic(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()

	if err := b.Write(ctx, "/notes.md", "hello world"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := b.Read(ctx, "/notes.md", 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "hello world" {
		t.Errorf("read content = %q, want %q", got, "hello world")
	}
}

func TestInMemory_ReadBeforeWriteEnforced(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()
	// 第一次写新建,允许直接写(无 read 要求)
	if err := b.Write(ctx, "/a.md", "v1"); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// 新 backend 实例(模拟跨 turn):预灌入 /a.md=v1,readPaths 为空,
	// 之后想覆盖必须先 read
	b2 := NewInMemoryBackend()
	b2.Preload(map[string]string{"/a.md": "v1"})
	err := b2.Write(ctx, "/a.md", "v2")
	if !errors.Is(err, ErrMustReadFirst) {
		t.Fatalf("want ErrMustReadFirst, got %v", err)
	}

	// Read 后允许 Write
	if _, err := b2.Read(ctx, "/a.md", 0, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := b2.Write(ctx, "/a.md", "v2"); err != nil {
		t.Errorf("after read, write should succeed: %v", err)
	}
}

func TestInMemory_EditFile(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()
	_ = b.Write(ctx, "/code.go", "func foo() {}\nfunc foo() {}\n")
	// Write 自动标记已读,允许后续 edit。但 edit_file 必须 old != new
	if _, err := b.Edit(ctx, "/code.go", "foo", "foo", false); !errors.Is(err, ErrSameOldAndNew) {
		t.Errorf("want ErrSameOldAndNew, got %v", err)
	}
	// 两处出现 + replace_all=false → ambiguous
	if _, err := b.Edit(ctx, "/code.go", "foo", "bar", false); !errors.Is(err, ErrEditAmbiguous) {
		t.Errorf("want ErrEditAmbiguous, got %v", err)
	}
	// replace_all=true → 全替
	n, err := b.Edit(ctx, "/code.go", "foo", "bar", true)
	if err != nil {
		t.Fatalf("edit replace_all: %v", err)
	}
	if n != 2 {
		t.Errorf("replaced count = %d, want 2", n)
	}
	got, _ := b.Read(ctx, "/code.go", 0, 0)
	if !strings.Contains(got, "bar()") || strings.Contains(got, "foo()") {
		t.Errorf("after replace_all, content = %q", got)
	}
}

func TestInMemory_EditFile_RequiresRead(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()
	b.Preload(map[string]string{"/a.md": "hello world"})
	// 没 read 直接 edit
	_, err := b.Edit(ctx, "/a.md", "hello", "hi", false)
	if !errors.Is(err, ErrMustReadFirst) {
		t.Errorf("want ErrMustReadFirst, got %v", err)
	}
}

func TestInMemory_EditFile_NoMatch(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()
	_ = b.Write(ctx, "/a.md", "hello")
	_, err := b.Edit(ctx, "/a.md", "missing", "x", false)
	if !errors.Is(err, ErrEditNoMatch) {
		t.Errorf("want ErrEditNoMatch, got %v", err)
	}
}

func TestInMemory_Ls(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()
	_ = b.Write(ctx, "/research/o3.md", "x")
	_ = b.Write(ctx, "/research/sonnet.md", "y")
	_ = b.Write(ctx, "/plan.md", "z")

	rootEntries, err := b.Ls(ctx, "/")
	if err != nil {
		t.Fatalf("ls /: %v", err)
	}
	// 期望:虚拟目录 /research(IsDir=true)+ 文件 /plan.md(IsDir=false)
	if len(rootEntries) != 2 {
		t.Fatalf("root entries: %d, want 2; got %+v", len(rootEntries), rootEntries)
	}
	if !rootEntries[0].IsDir || rootEntries[0].Path != "/research" {
		t.Errorf("[0] = %+v, want /research dir", rootEntries[0])
	}
	if rootEntries[1].IsDir || rootEntries[1].Path != "/plan.md" {
		t.Errorf("[1] = %+v, want /plan.md file", rootEntries[1])
	}

	dirEntries, err := b.Ls(ctx, "/research")
	if err != nil {
		t.Fatalf("ls /research: %v", err)
	}
	if len(dirEntries) != 2 {
		t.Fatalf("research entries: %d, want 2", len(dirEntries))
	}
}

func TestInMemory_PathValidation(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()
	cases := []struct {
		name string
		path string
	}{
		{"no leading slash", "notes.md"},
		{"empty", ""},
		{"double slash", "/foo//bar.md"},
		{"path traversal", "/foo/../bar.md"},
		{"dot segment", "/foo/./bar.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := b.Write(ctx, tc.path, "x")
			if !errors.Is(err, ErrInvalidPath) {
				t.Errorf("path %q: want ErrInvalidPath, got %v", tc.path, err)
			}
		})
	}
}

func TestInMemory_FileTooLarge(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()
	huge := strings.Repeat("x", MaxFileBytes+1)
	err := b.Write(ctx, "/big.bin", huge)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("want ErrFileTooLarge, got %v", err)
	}
}

func TestInMemory_ReadPagination(t *testing.T) {
	b := NewInMemoryBackend()
	ctx := context.Background()
	lines := make([]string, 0, 10)
	for i := 1; i <= 10; i++ {
		lines = append(lines, "line"+string(rune('0'+i-1)))
	}
	content := strings.Join(lines, "\n")
	_ = b.Write(ctx, "/x.txt", content)

	// 默认从 line 1 全读
	got, _ := b.Read(ctx, "/x.txt", 0, 0)
	if got != content {
		t.Errorf("full read mismatch")
	}
	// 从第 3 行起读 4 行
	got, _ = b.Read(ctx, "/x.txt", 3, 4)
	want := strings.Join(lines[2:6], "\n")
	if got != want {
		t.Errorf("paginated read = %q, want %q", got, want)
	}
	// offset 超界返空
	got, _ = b.Read(ctx, "/x.txt", 100, 0)
	if got != "" {
		t.Errorf("out-of-range offset = %q, want empty", got)
	}
}
