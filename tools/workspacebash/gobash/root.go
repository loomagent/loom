package gobash

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// toRootRel 把模型给的逻辑路径(绝对 /workspace/x、绝对 /x、相对 x)统一成 os.Root
// 相对路径(无前导斜杠)。
//
// 历史上 agent-sandbox daemon 把 workspace 挂在 /workspace,react 的 guide 文案用
// /workspace/... 绝对路径,proreport 用相对路径,两者都要落到同一个 workspace 根。
// os.Root 自身会拒 ../ / 绝对路径 / symlink 逃逸,这里只做前缀归一,安全兜底交给 os.Root。
func toRootRel(dir, p string) string {
	if p == "" {
		p = dir
	}
	if !strings.HasPrefix(p, "/") {
		// 相对路径:接到逻辑 cwd(dir,通常 "/")后面。
		p = path.Join(dir, p)
	}
	p = path.Clean(p)
	// 兼容 daemon 时代的 /workspace 挂载点前缀。
	if p == "/workspace" {
		p = "/"
	} else if rest, ok := strings.CutPrefix(p, "/workspace/"); ok {
		p = "/" + rest
	}
	rel := strings.TrimPrefix(p, "/")
	if rel == "" {
		return "."
	}
	return path.Clean(rel)
}

// openFile 经 os.Root 打开一个只读文件。路径先归一,再由 os.Root 强制封闭在 workspace 根内。
func openFile(env CmdEnv, arg string) (*os.File, error) {
	return env.Root.Open(toRootRel(env.Dir, arg))
}

// newOpenHandler 是 interp 的只读 OpenHandler:命令替换 $(<f) / 输入重定向 / >/dev/null
// 都走它(写重定向 validator 已拦,这里二层兜底)。
func newOpenHandler(root *os.Root, dir string) func(context.Context, string, int, os.FileMode) (io.ReadWriteCloser, error) {
	return func(_ context.Context, p string, flag int, _ os.FileMode) (io.ReadWriteCloser, error) {
		if p == "/dev/null" {
			return devNull{}, nil
		}
		// 任何写 flag 一律拒绝:workspace 对 LLM 恒只读。
		if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC) != 0 {
			return nil, fs.ErrPermission
		}
		f, err := root.Open(toRootRel(dir, p))
		if err != nil {
			return nil, err
		}
		return f, nil
	}
}

// newStatHandler 是 interp 的 StatHandler(test -f / -d 等用)。不跟随 symlink 逃逸:
// os.Root 本就拒 symlink 逃逸,followSymlinks 参数仅决定用 Stat 还是 Lstat。
func newStatHandler(root *os.Root, dir string) func(context.Context, string, bool) (fs.FileInfo, error) {
	return func(_ context.Context, name string, followSymlinks bool) (fs.FileInfo, error) {
		rel := toRootRel(dir, name)
		if followSymlinks {
			return root.Stat(rel)
		}
		return root.Lstat(rel)
	}
}

// newReadDirHandler 是 interp 的 ReadDirHandler2:glob 展开(raw/SRC-*.md 等高频用法)
// 依赖它列目录。必须实现,否则 glob 静默失效。
func newReadDirHandler(root *os.Root, dir string) func(context.Context, string) ([]fs.DirEntry, error) {
	return func(_ context.Context, p string) ([]fs.DirEntry, error) {
		return fs.ReadDir(root.FS(), toRootRel(dir, p))
	}
}

// devNull 实现 io.ReadWriteCloser:读即 EOF,写即丢弃。给 >/dev/null、2>/dev/null 用。
type devNull struct{}

func (devNull) Read([]byte) (int, error)    { return 0, io.EOF }
func (devNull) Write(p []byte) (int, error) { return len(p), nil }
func (devNull) Close() error                { return nil }
