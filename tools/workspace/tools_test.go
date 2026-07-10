package workspace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/loomagent/loom"
)

func TestTool_WriteThenRead(t *testing.T) {
	b := NewInMemoryBackend()
	w := NewWriteFile(b)
	r := NewReadFile(b)
	ctx := context.Background()

	out, err := w.Invoke(ctx, `{"path":"/a.md","content":"hello\nworld"}`)
	if err != nil {
		t.Fatalf("write invoke: %v", err)
	}
	var wResult struct {
		Path  string `json:"path"`
		Bytes int    `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(out), &wResult); err != nil {
		t.Fatalf("write result not JSON: %v; out=%q", err, out)
	}
	if wResult.Path != "/a.md" || wResult.Bytes != 11 {
		t.Errorf("write result = %+v", wResult)
	}

	rOut, err := r.Invoke(ctx, `{"path":"/a.md"}`)
	if err != nil {
		t.Fatalf("read invoke: %v", err)
	}
	// 期望:含 cat -n 风格行号
	if !strings.Contains(rOut, "     1→hello") {
		t.Errorf("read output missing line number prefix: %q", rOut)
	}
	if !strings.Contains(rOut, "     2→world") {
		t.Errorf("read output missing line 2: %q", rOut)
	}
}

func TestTool_LsRoot(t *testing.T) {
	b := NewInMemoryBackend()
	w := NewWriteFile(b)
	l := NewLs(b)
	ctx := context.Background()
	if _, err := w.Invoke(ctx, `{"path":"/x.md","content":"a"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Invoke(ctx, `{"path":"/sub/y.md","content":"b"}`); err != nil {
		t.Fatal(err)
	}
	out, err := l.Invoke(ctx, `{"path":"/"}`)
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	var parsed struct {
		Entries []struct {
			Path  string `json:"path"`
			IsDir bool   `json:"is_dir"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("ls result not JSON: %v; out=%q", err, out)
	}
	if len(parsed.Entries) != 2 {
		t.Fatalf("entries: %d, want 2; got %+v", len(parsed.Entries), parsed.Entries)
	}
	if !parsed.Entries[0].IsDir || parsed.Entries[0].Path != "/sub" {
		t.Errorf("[0] = %+v, want /sub dir", parsed.Entries[0])
	}
	if parsed.Entries[1].IsDir || parsed.Entries[1].Path != "/x.md" {
		t.Errorf("[1] = %+v, want /x.md file", parsed.Entries[1])
	}
}

func TestTool_EditFile_HappyPath(t *testing.T) {
	b := NewInMemoryBackend()
	w := NewWriteFile(b)
	e := NewEditFile(b)
	ctx := context.Background()
	_, _ = w.Invoke(ctx, `{"path":"/a.md","content":"hello world"}`)
	out, err := e.Invoke(ctx, `{"path":"/a.md","old_string":"world","new_string":"universe"}`)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	var res struct {
		Path     string `json:"path"`
		Replaced int    `json:"replaced"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("edit result not JSON: %v", err)
	}
	if res.Replaced != 1 {
		t.Errorf("replaced = %d, want 1", res.Replaced)
	}
	// 确认内容真的改了
	got, _ := b.Read(ctx, "/a.md", 0, 0)
	if got != "hello universe" {
		t.Errorf("after edit: %q", got)
	}
}

func TestTool_RegisterAll(t *testing.T) {
	b := NewInMemoryBackend()
	reg := loom.NewToolRegistry()
	if err := RegisterAll(reg, b); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{ToolNameLs, ToolNameReadFile, ToolNameWriteFile, ToolNameEditFile} {
		if _, ok := reg.Lookup(name); !ok {
			t.Errorf("tool %q not found in registry", name)
		}
	}
}
