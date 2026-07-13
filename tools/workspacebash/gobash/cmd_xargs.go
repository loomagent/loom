package gobash

import (
	"context"
	"io"
	"strconv"
	"strings"
)

// cmdXargs 把 stdin 的 token 当参数喂给一条只读命令。支持 -0(NUL 分隔)、-n N(每次最多 N 个)、
// -I REPL(逐 token 替换)。CMD 必须在 registry(等价于在 allowlist),否则 127——这是 xargs
// 唯一能"再触发命令"的口子,必须卡死在白名单内,不开 sh -c 之类逃逸。
func cmdXargs(ctx context.Context, env CmdEnv, args []string) int {
	nullDelim := false
	maxArgs := 0
	replStr := ""
	// 选项解析止于首个非选项 token(= CMD),其后原样作为命令与初始参数。
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "-0" || a == "--null" {
			nullDelim = true
			continue
		}
		if a == "-n" || a == "--max-args" {
			if i+1 < len(args) {
				i++
				maxArgs, _ = strconv.Atoi(args[i])
			}
			continue
		}
		if rest, ok := strings.CutPrefix(a, "-n"); ok && rest != "" {
			maxArgs, _ = strconv.Atoi(rest)
			continue
		}
		if a == "-I" {
			if i+1 < len(args) {
				i++
				replStr = args[i]
			}
			continue
		}
		if rest, ok := strings.CutPrefix(a, "-I"); ok && rest != "" {
			replStr = rest
			continue
		}
		break // 首个非选项:CMD
	}
	cmdArgs := args[i:]
	if len(cmdArgs) == 0 {
		io.WriteString(env.Stderr, "xargs: 缺少命令(只读 workspace 不支持默认 echo,必须显式给命令)\n")
		return exitUsageError
	}
	cmdName := cmdArgs[0]
	initial := cmdArgs[1:]
	fn, ok := registry[cmdName]
	if !ok {
		io.WriteString(env.Stderr, "xargs: "+cmdName+": command not available in read-only workspace\n")
		return exitNotAvailable
	}

	tokens, err := readTokens(ctx, env.Stdin, nullDelim)
	if err != nil {
		return ioErrExit(env, "xargs", err)
	}

	childEnv := env
	childEnv.Stdin = strings.NewReader("") // xargs 的子命令从参数取输入,不继承 stdin

	failed := false
	invoke := func(extra []string) bool {
		if cerr := ctx.Err(); cerr != nil {
			return false
		}
		full := append(append([]string{}, initial...), extra...)
		if runCmd(ctx, fn, childEnv, full) != exitOK {
			failed = true
		}
		return true
	}

	switch {
	case replStr != "":
		// -I:逐 token 替换 initial 里的 REPL,每个 token 跑一次。
		for _, tok := range tokens {
			replaced := make([]string, len(initial))
			for k, a := range initial {
				replaced[k] = strings.ReplaceAll(a, replStr, tok)
			}
			if !invokeReplaced(ctx, fn, childEnv, replaced, &failed) {
				return exitTimedOut
			}
		}
	case maxArgs > 0:
		for start := 0; start < len(tokens); start += maxArgs {
			end := min(start+maxArgs, len(tokens))
			if !invoke(tokens[start:end]) {
				return exitTimedOut
			}
		}
	default:
		if len(tokens) == 0 {
			break // 无输入且非 -I:GNU xargs 默认仍跑一次,但只读场景跑一次空参无意义,跳过
		}
		if !invoke(tokens) {
			return exitTimedOut
		}
	}

	if failed {
		return 123 // GNU xargs:子命令有非零退出
	}
	return exitOK
}

func invokeReplaced(ctx context.Context, fn CmdFunc, env CmdEnv, full []string, failed *bool) bool {
	if cerr := ctx.Err(); cerr != nil {
		return false
	}
	if runCmd(ctx, fn, env, full) != exitOK {
		*failed = true
	}
	return true
}

// readTokens 把 stdin 按空白(或 -0 的 NUL)切成 token,受 maxReadBytes 约束。
func readTokens(ctx context.Context, r io.Reader, nullDelim bool) ([]string, error) {
	data, err := readCapped(r)
	if err != nil {
		return nil, err
	}
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	s := string(data)
	if nullDelim {
		parts := strings.Split(s, "\x00")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	}
	return strings.Fields(s), nil
}
