package gobash

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"mvdan.cc/sh/v3/interp"

	"github.com/loomagent/loom/tools/workspacebash"
)

// exit code 约定(对齐 coreutils / shell 惯例)。
const (
	exitOK           = 0
	exitGenericError = 1
	exitUsageError   = 2
	exitTimedOut     = 124 // 墙钟超时被终止
	exitCommandPanic = 125 // 命令实现 panic(越界/nil 等),被 recover 兜住
	exitNotAvailable = 127 // 命令不在 registry(allowlist 漏网 / 未实现)
)

// CmdEnv 是命令实现的运行上下文。FS 访问只准经 Root(os.Root 强制封闭在 workspace 根内,
// 只读由"绝不调 Create/Write"构造性保证)。
type CmdEnv struct {
	Root   *os.Root  // workspace 根句柄,所有文件访问的唯一入口
	Dir    string    // interp 的逻辑 cwd(通常 "/")
	Stdin  io.Reader // 上游输入
	Stdout io.Writer // 下游输出
	Stderr io.Writer
}

// CmdFunc 是一条进程内命令的实现。返回 exit code(非 error):命令级失败用 exit code 表达,
// 走 stderr 给原因;返回的 Go error 仅用于无法继续的内部异常(极少)。
type CmdFunc func(ctx context.Context, env CmdEnv, args []string) int

// registry 是命令名 → 实现的映射。allowlist 必须与它完全一致(下方 init 断言)。
// 在 init 里填表(而非 var 字面量)以打破静态初始化环:cmdXargs 反过来引用 registry。
var registry map[string]CmdFunc

// init 填表并校验 allowlist ⊆ registry:validator 放行的命令必须都有进程内实现,否则 dispatch
// 中间件会把它判成 127。这条断言在进程启动期暴露"放行了但没实现"的漂移,而非运行期才发现。
func init() {
	registry = map[string]CmdFunc{
		"cat":   cmdCat,
		"head":  cmdHead,
		"tail":  cmdTail,
		"grep":  cmdGrep,
		"jq":    cmdJq,
		"ls":    cmdLs,
		"wc":    cmdWc,
		"sort":  cmdSort,
		"uniq":  cmdUniq,
		"cut":   cmdCut,
		"find":  cmdFind,
		"nl":    cmdNl,
		"pwd":   cmdPwd,
		"sed":   cmdSed,
		"stat":  cmdStat,
		"file":  cmdFile,
		"tr":    cmdTr,
		"xargs": cmdXargs,
	}
	var missing []string
	for _, name := range workspacebash.DefaultAllowlist {
		if _, ok := registry[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		panic(fmt.Sprintf("gobash: allowlist 命令缺少进程内实现: %v(registry 与 workspacebash.DefaultAllowlist 必须一致)", missing))
	}
}

// dispatchMiddleware 是 interp 的 ExecHandler 中间件:命中 registry 就用进程内实现,
// 否则显式返回 127——**绝不 next 到宿主真二进制**(fallthrough 会逃逸出沙盒,安全关键)。
func dispatchMiddleware(root *os.Root) func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(_ interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return interp.ExitStatus(exitUsageError)
			}
			hc := interp.HandlerCtx(ctx)
			fn, ok := registry[args[0]]
			if !ok {
				fmt.Fprintf(hc.Stderr, "%s: command not available in read-only workspace\n", args[0])
				return interp.ExitStatus(exitNotAvailable)
			}
			// ctx 已被取消(超时)则直接判超时码,不再启动命令。
			if err := ctx.Err(); err != nil {
				return interp.ExitStatus(exitTimedOut)
			}
			code := runCmd(ctx, fn, CmdEnv{
				Root:   root,
				Dir:    hc.Dir,
				Stdin:  hc.Stdin,
				Stdout: hc.Stdout,
				Stderr: hc.Stderr,
			}, args[1:])
			if code != exitOK {
				return interp.ExitStatus(uint8(code))
			}
			return nil
		}
	}
}

// runCmd 包一层 recover:单条命令 panic 不打挂整个 controller goroutine,转成 exit 125。
func runCmd(ctx context.Context, fn CmdFunc, env CmdEnv, args []string) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(env.Stderr, "internal error: %v\n", r)
			code = exitCommandPanic
		}
	}()
	return fn(ctx, env, args)
}
