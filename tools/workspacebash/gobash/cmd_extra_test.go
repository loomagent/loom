package gobash

import (
	"strings"
	"testing"
)

func TestSed_LineWindow(t *testing.T) {
	r := newTestRunner(t, map[string]string{
		"doc.md": "L1\nL2\nL3\nL4\nL5\n",
	})
	if got := run(t, r, `sed -n '2,4p' doc.md`).Stdout; got != "L2\nL3\nL4\n" {
		t.Fatalf("sed 2,4p = %q", got)
	}
	if got := run(t, r, `sed -n '3p' doc.md`).Stdout; got != "L3\n" {
		t.Fatalf("sed 3p = %q", got)
	}
	if got := run(t, r, `sed -n '4,$p' doc.md`).Stdout; got != "L4\nL5\n" {
		t.Fatalf("sed 4,$p = %q", got)
	}
}

func TestSed_RejectsSubstitution(t *testing.T) {
	r := newTestRunner(t, map[string]string{"doc.md": "hello\n"})
	res := run(t, r, `sed -n 's/hello/bye/p' doc.md`)
	if res.ExitCode != exitUsageError {
		t.Fatalf("sed s/// 应被拒,exit=%d", res.ExitCode)
	}
}

func TestTr_DeleteAndTranslate(t *testing.T) {
	r := newTestRunner(t, map[string]string{"d.txt": "Hello World\n"})
	if got := run(t, r, `cat d.txt | tr 'a-z' 'A-Z'`).Stdout; got != "HELLO WORLD\n" {
		t.Fatalf("tr upcase = %q", got)
	}
	if got := run(t, r, `cat d.txt | tr -d 'lo'`).Stdout; got != "He Wrd\n" {
		t.Fatalf("tr -d = %q", got)
	}
}

func TestXargs_ReplaceInvokesRegistry(t *testing.T) {
	r := newTestRunner(t, sampleWorkspace)
	// find 出 raw 下文件 → xargs -I{} grep market {}:每个文件查一次。
	res := run(t, r, `find raw -name SRC-*.md | xargs -I{} grep -l market {}`)
	if !strings.Contains(res.Stdout, "raw/SRC-1.md") {
		t.Fatalf("xargs -I{} grep -l = %q", res.Stdout)
	}
}

func TestXargs_RejectsUnlistedCommand(t *testing.T) {
	r := newTestRunner(t, map[string]string{"x.txt": "a\nb\n"})
	res := run(t, r, `cat x.txt | xargs curl`)
	if res.ExitCode != exitNotAvailable {
		t.Fatalf("xargs 调未登记命令应 127,exit=%d", res.ExitCode)
	}
}

func TestStat(t *testing.T) {
	r := newTestRunner(t, map[string]string{"f.txt": "12345"})
	if got := run(t, r, `stat -c %s f.txt`).Stdout; strings.TrimSpace(got) != "5" {
		t.Fatalf("stat -c %%s = %q", got)
	}
}
