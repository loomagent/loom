package gobash

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loomagent/loom/tools/workspacebash"
)

// newTestRunner 建一个临时 workspace,写入 files(path→content),返回 runner。
func newTestRunner(t *testing.T, files map[string]string) *Runner {
	t.Helper()
	dir := t.TempDir()
	for p, content := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	r, err := New(Options{WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func run(t *testing.T, r *Runner, command string) workspacebash.Result {
	t.Helper()
	res, err := r.Run(context.Background(), command)
	if err != nil {
		t.Fatalf("Run(%q): %v", command, err)
	}
	return res
}

var sampleWorkspace = map[string]string{
	"sources.jsonl": `{"src_id":"SRC-1","url":"https://a.com","summary":"alpha market"}
{"src_id":"SRC-2","url":"https://b.com","summary":"beta growth"}
{"src_id":"SRC-3","url":"https://c.com","summary":"gamma market share"}
`,
	"queries.jsonl": `{"query_id":"Q-1","query":"market size","tool":"web_search"}
{"query_id":"Q-2","query":"growth rate","tool":"web_search"}
`,
	"raw/SRC-1.md": "# Alpha\nthe market is large\nrevenue grew 20%\n",
	"raw/SRC-2.md": "# Beta\ngrowth was steady\nno market mention here\n",
	"raw/SRC-3.md": "# Gamma\nmarket share rose\n",
}

func TestJq_JSONLStreaming(t *testing.T) {
	r := newTestRunner(t, sampleWorkspace)
	res := run(t, r, `jq -r '.src_id + " " + .url' sources.jsonl`)
	want := "SRC-1 https://a.com\nSRC-2 https://b.com\nSRC-3 https://c.com\n"
	if res.Stdout != want {
		t.Fatalf("jq stdout=%q want %q", res.Stdout, want)
	}
	if res.ExitCode != 0 {
		t.Fatalf("jq exit=%d", res.ExitCode)
	}
}

func TestJq_AbsoluteWorkspacePath(t *testing.T) {
	r := newTestRunner(t, sampleWorkspace)
	// react 文案用 /workspace/... 绝对路径,必须能落到根。
	res := run(t, r, `jq -r .query /workspace/queries.jsonl`)
	if res.Stdout != "market size\ngrowth rate\n" {
		t.Fatalf("jq abs-path stdout=%q", res.Stdout)
	}
}

func TestGrep_RecursiveWithLineNo(t *testing.T) {
	r := newTestRunner(t, sampleWorkspace)
	res := run(t, r, `grep -rn market raw`)
	// 期望命中 SRC-1(line2)、SRC-2(line3 "no market mention")、SRC-3(line2),带文件名+行号前缀。
	for _, want := range []string{"raw/SRC-1.md:2:", "raw/SRC-3.md:2:"} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("grep -rn missing %q in:\n%s", want, res.Stdout)
		}
	}
	if res.ExitCode != 0 {
		t.Fatalf("grep exit=%d", res.ExitCode)
	}
}

func TestGrep_GlobExpansion(t *testing.T) {
	r := newTestRunner(t, sampleWorkspace)
	// glob raw/SRC-*.md 由 interp 经 ReadDirHandler 展开,必须工作。
	res := run(t, r, `grep -l market raw/SRC-*.md`)
	if !strings.Contains(res.Stdout, "raw/SRC-1.md") || !strings.Contains(res.Stdout, "raw/SRC-3.md") {
		t.Fatalf("grep -l glob stdout=%q", res.Stdout)
	}
}

func TestGrep_NoMatchExit1(t *testing.T) {
	r := newTestRunner(t, sampleWorkspace)
	res := run(t, r, `grep zzznotfound sources.jsonl`)
	if res.ExitCode != 1 {
		t.Fatalf("grep no-match exit=%d want 1", res.ExitCode)
	}
}

func TestPipeline_JqHeadWc(t *testing.T) {
	r := newTestRunner(t, sampleWorkspace)
	res := run(t, r, `jq -r .src_id sources.jsonl | head -n 2 | wc -l`)
	if strings.TrimSpace(res.Stdout) != "2" {
		t.Fatalf("pipeline stdout=%q want 2", res.Stdout)
	}
}

func TestCatHeadTail(t *testing.T) {
	r := newTestRunner(t, sampleWorkspace)
	if got := run(t, r, `head -n 1 raw/SRC-1.md`).Stdout; got != "# Alpha\n" {
		t.Fatalf("head=%q", got)
	}
	if got := run(t, r, `tail -n 1 raw/SRC-1.md`).Stdout; got != "revenue grew 20%\n" {
		t.Fatalf("tail=%q", got)
	}
}

func TestSortUniqCut(t *testing.T) {
	r := newTestRunner(t, map[string]string{
		"data.tsv": "b\t2\na\t1\nb\t2\nc\t3\n",
	})
	if got := run(t, r, `cut -f1 data.tsv | sort | uniq -c`).Stdout; !strings.Contains(got, "a") || !strings.Contains(got, "2 b") {
		t.Fatalf("sort|uniq -c =%q", got)
	}
}

func TestFind_NameType(t *testing.T) {
	r := newTestRunner(t, map[string]string{
		"raw/SRC-1.md":             "source",
		"raw/notes.txt":            "notes",
		"raw/SRC-directory.md/doc": "nested",
	})
	res := run(t, r, `find raw -type f -name SRC-*.md`)
	if res.ExitCode != 0 {
		t.Fatalf("find exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if res.Stdout != "raw/SRC-1.md\n" {
		t.Fatalf("find stdout=%q want %q", res.Stdout, "raw/SRC-1.md\n")
	}
}

func TestFind_InvalidNamePattern(t *testing.T) {
	r := newTestRunner(t, sampleWorkspace)
	res := run(t, r, `find raw -name '['`)
	if res.ExitCode != exitUsageError {
		t.Fatalf("find invalid pattern exit=%d want %d", res.ExitCode, exitUsageError)
	}
	if res.Stdout != "" {
		t.Fatalf("find invalid pattern stdout=%q want empty", res.Stdout)
	}
	if !strings.Contains(res.Stderr, `find: invalid -name pattern "[":`) {
		t.Fatalf("find invalid pattern stderr=%q", res.Stderr)
	}
}

func TestPwd(t *testing.T) {
	r := newTestRunner(t, sampleWorkspace)
	if got := run(t, r, `pwd`).Stdout; got != "/\n" {
		t.Fatalf("pwd=%q", got)
	}
}
