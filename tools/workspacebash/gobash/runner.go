// Package gobash 是 workspace bash 工具的**纯 Go 进程内**只读执行后端,替代历史上的
// agent-sandbox gRPC daemon(bubblewrap)。
//
// 隔离不再靠内核(无 daemon、无 bubblewrap、跨平台):
//   - 命令白名单:只有 registry 里登记的命令能跑,未登记一律 exit 127,**绝不**逃逸到宿主真二进制;
//   - 文件封闭:所有 FS 访问经 os.Root(Go 1.26),自动拒 ../ / 绝对路径 / symlink 逃逸;
//   - 只读:命令实现只读不写 + OpenHandler 拒一切写 flag(workspace 对 LLM 恒只读不变量);
//   - shell 语义:mvdan/sh interp 提供管道/逻辑/glob/引用,命令实现只管单命令算法。
//
// 残留风险(诚实标注):进程内无法像 cgroup 那样硬限内存——sort/jq 全量加载大文件时
// 峰值内存只有"单命令读字节上限"这个软闸,先读后判,峰值仍可能短暂超。墙钟超时与输出
// 上限是硬的。威胁模型已收缩到"只跑模型对自己 workspace 的只读查询",workspace 体量本身受控。
package gobash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/loomagent/loom/tools/workspacebash"
)

const (
	defaultTimeout   = 20 * time.Second
	defaultMaxStdout = 200 * 1024
	defaultMaxStderr = 16 * 1024
)

// Options 配置一个进程内 runner。无 endpoint / workspace_id——不再有 daemon 要拨号/绑定。
type Options struct {
	// WorkspaceDir 必填,workspace 本地根目录。os.OpenRoot 在 New 期打开,目录不存在即构造失败。
	WorkspaceDir string
	// Timeout 单条命令墙钟超时;<=0 用默认 20s。
	Timeout time.Duration
	// MaxStdout / MaxStderr 输出字节上限;0 用默认。超限截断并置 Truncated。
	MaxStdout uint64
	MaxStderr uint64
}

// Runner 是进程内 bash 执行器,满足 workspacebash.Runner。
type Runner struct {
	root      *os.Root
	timeout   time.Duration
	maxStdout uint64
	maxStderr uint64
}

// New 打开 workspace 根句柄并构造 runner。WorkspaceDir 不存在 → os.OpenRoot 失败 → 返错
// (react 工具级报错 / proreport turn 级 fail-fast,与 daemon 时代构造失败语义一致)。
func New(opts Options) (*Runner, error) {
	dir := strings.TrimSpace(opts.WorkspaceDir)
	if dir == "" {
		return nil, errors.New("gobash: workspace_dir 不能为空")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("gobash: 打开 workspace 根 %q: %w", dir, err)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxStdout := opts.MaxStdout
	if maxStdout == 0 {
		maxStdout = defaultMaxStdout
	}
	maxStderr := opts.MaxStderr
	if maxStderr == 0 {
		maxStderr = defaultMaxStderr
	}
	return &Runner{root: root, timeout: timeout, maxStdout: maxStdout, maxStderr: maxStderr}, nil
}

// Close 释放 workspace 根句柄。
func (r *Runner) Close() error {
	if r == nil || r.root == nil {
		return nil
	}
	return r.root.Close()
}

// Run 在进程内只读执行一条命令(命令已由 workspacebash.Validator 校验过 allowlist 与危险构造)。
// 超时/截断/退出码是结构化结果而非 Go error;Go error 仅用于无法解析/无法构造的内部异常。
func (r *Runner) Run(ctx context.Context, command string) (workspacebash.Result, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return workspacebash.Result{}, fmt.Errorf("gobash: 解析命令: %w", err)
	}

	stdout := newLimitedBuffer(r.maxStdout)
	stderr := newLimitedBuffer(r.maxStderr)

	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	ir, err := interp.New(
		interp.Dir("/"),
		interp.StdIO(bytes.NewReader(nil), stdout, stderr),
		interp.ExecHandlers(dispatchMiddleware(r.root)),
		interp.OpenHandler(newOpenHandler(r.root, "/")),
		interp.StatHandler(newStatHandler(r.root, "/")),
		interp.ReadDirHandler2(newReadDirHandler(r.root, "/")),
	)
	if err != nil {
		return workspacebash.Result{}, fmt.Errorf("gobash: 构造 interp: %w", err)
	}

	runErr := ir.Run(runCtx, file)

	res := workspacebash.Result{
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
	}
	res.ExitCode, res.TimedOut = classifyRunError(runCtx, runErr)
	return res, nil
}

// classifyRunError 把 interp.Run 的返回值翻译成 (exitCode, timedOut)。
func classifyRunError(ctx context.Context, err error) (uint64, bool) {
	// 超时优先:ctx 截止 → exit 124 + TimedOut。
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return exitTimedOut, true
	}
	if err == nil {
		return exitOK, false
	}
	var status interp.ExitStatus
	if errors.As(err, &status) {
		return uint64(status), false
	}
	// 其它内部错误(interp 异常)落成 exit 1。
	return exitGenericError, false
}
