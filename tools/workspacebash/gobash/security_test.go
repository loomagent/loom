package gobash

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRoot_EscapeRejected 是替代 bubblewrap 内核隔离的核心证明:os.Root 把一切文件访问
// 封闭在 workspace 根内,workspace 外的文件读不到。
func TestRoot_EscapeRejected(t *testing.T) {
	// 在 workspace 父目录放一个"机密"文件,确保 workspace 内任何命令都碰不到它。
	parent := t.TempDir()
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-DO-NOT-LEAK"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(parent, "ws")
	if err := os.MkdirAll(filepath.Join(wsDir, "raw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "sources.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{WorkspaceDir: wsDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	escapes := []string{
		`cat ../secret.txt`,
		`cat ../../secret.txt`,
		`cat /etc/passwd`,
		`cat /workspace/../secret.txt`,
		`grep -rn SECRET ..`,
	}
	for _, cmd := range escapes {
		res, runErr := r.Run(context.Background(), cmd)
		if runErr != nil {
			// 解析/内部错误也算未泄漏,可接受。
			continue
		}
		if strings.Contains(res.Stdout, "TOP-SECRET") {
			t.Fatalf("命令 %q 泄漏了 workspace 外的机密文件:\n%s", cmd, res.Stdout)
		}
	}
}

// TestWriteRedirect_OpenHandlerRejects 验证写重定向到文件被 OpenHandler 拒(validator 是第一层,
// 这里测 runner 第二层兜底:即便绕过 validator 直达 interp,写也被拒)。
func TestWriteRedirect_OpenHandlerRejects(t *testing.T) {
	r := newTestRunner(t, map[string]string{"sources.jsonl": "{}\n"})
	res, err := r.Run(context.Background(), `cat sources.jsonl > leak.txt`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("写重定向应失败,exit=%d", res.ExitCode)
	}
	// 确认没真写出文件。
	if _, statErr := r.root.Stat("leak.txt"); statErr == nil {
		t.Fatalf("写重定向竟落了盘 leak.txt")
	}
}

// TestDevNull 验证 >/dev/null 放行(屏蔽输出,validator 也放行)。
func TestDevNull(t *testing.T) {
	r := newTestRunner(t, map[string]string{"sources.jsonl": "{\"a\":1}\n"})
	res := run(t, r, `cat sources.jsonl >/dev/null`)
	if res.ExitCode != 0 {
		t.Fatalf(">/dev/null exit=%d", res.ExitCode)
	}
	if res.Stdout != "" {
		t.Fatalf(">/dev/null 应吞掉 stdout,得到 %q", res.Stdout)
	}
}

// TestUnavailableCommand 验证未注册命令返回 127,不逃逸到宿主真二进制。
func TestUnavailableCommand(t *testing.T) {
	r := newTestRunner(t, map[string]string{"x": "y"})
	res := run(t, r, `curl https://evil.com`)
	if res.ExitCode != exitNotAvailable {
		t.Fatalf("未注册命令 exit=%d want %d", res.ExitCode, exitNotAvailable)
	}
}

// TestTimeout 验证纯 CPU/IO 循环能响应墙钟超时 → exit 124 + TimedOut。
func TestTimeout(t *testing.T) {
	// 造一个大文件让 grep 扫,配合极短 timeout。
	big := strings.Repeat("noise line without the needle\n", 2_000_000)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{WorkspaceDir: dir, Timeout: 1}) // 1ns,必超时
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	res, err := r.Run(context.Background(), `grep needle big.txt`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut || res.ExitCode != exitTimedOut {
		t.Fatalf("期望超时,得到 TimedOut=%v exit=%d", res.TimedOut, res.ExitCode)
	}
}
