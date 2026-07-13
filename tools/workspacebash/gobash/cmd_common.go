package gobash

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxReadBytes 单条命令从文件/stdin 累计读取的硬上限,防 sort/jq 把巨型文件读爆内存。
// 这是进程内相对 daemon 唯一的内存软闸(先读后判,峰值仍可能短暂超)。
const maxReadBytes = 64 * 1024 * 1024

// scanFlags 解析 [flags] [positionals]。
//   - valueShorts:取值的短 flag 字符集合(如 head 的 "nc")。
//   - valueLongs:取值的长 flag 名集合。
//
// 返回 set(出现过的 flag,短 flag 用单字符、长 flag 用全名 → true)、vals(取值 flag → 值列表,
// 可重复如 grep -e A -e B)、pos(位置参数)。支持组合短 flag(-rn)、附着值(-n5)、`--` 终止、
// `-` 当位置参数(stdin 标记)。不认识的 flag 宽松忽略(不入 set),调用方按需严格化。
func scanFlags(args []string, valueShorts string, valueLongs []string) (set map[string]bool, vals map[string][]string, pos []string) {
	set = map[string]bool{}
	vals = map[string][]string{}
	longVal := map[string]bool{}
	for _, l := range valueLongs {
		longVal[l] = true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			pos = append(pos, args[i+1:]...)
			return
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			pos = append(pos, arg)
		case strings.HasPrefix(arg, "--"):
			name := arg[2:]
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				key := name[:eq]
				set[key] = true
				vals[key] = append(vals[key], name[eq+1:])
			} else if longVal[name] {
				set[name] = true
				if i+1 < len(args) {
					i++
					vals[name] = append(vals[name], args[i])
				}
			} else {
				set[name] = true
			}
		default: // 短 flag 串,如 -rn / -n5 / -i
			chars := arg[1:]
			for j := 0; j < len(chars); j++ {
				c := chars[j]
				key := string(c)
				set[key] = true
				if strings.IndexByte(valueShorts, c) >= 0 {
					rest := chars[j+1:]
					if rest != "" {
						vals[key] = append(vals[key], rest)
					} else if i+1 < len(args) {
						i++
						vals[key] = append(vals[key], args[i])
					}
					break // 取值 flag 吃掉本 token 剩余
				}
			}
		}
	}
	return
}

// firstVal 返回取值 flag 的第一个值,没有则返回 def。
func firstVal(vals map[string][]string, keys ...string) (string, bool) {
	for _, k := range keys {
		if v := vals[k]; len(v) > 0 {
			return v[0], true
		}
	}
	return "", false
}

// numericShortFlag 在 set 里找一个纯数字短 flag(如 head -1 的 "1"),返回其数值。
// 用于支持 GNU 的 -NUM 行数简写。
func numericShortFlag(set map[string]bool) (int, bool) {
	for k := range set {
		if n, err := strconv.Atoi(k); err == nil {
			return n, true
		}
	}
	return 0, false
}

// anySet 返回任一别名 flag 是否出现(如 -i / --ignore-case)。
func anySet(set map[string]bool, keys ...string) bool {
	for _, k := range keys {
		if set[k] {
			return true
		}
	}
	return false
}

// namedReader 是一个带名字(用于多文件前缀)的输入流。
type namedReader struct {
	name string // 文件名;stdin 为 "-"
	r    io.Reader
}

// openInputs 把位置参数当文件名经 os.Root 打开;无位置参数时返回 stdin(名 "-")。
// 打开失败的文件写 stderr 并跳过(返回 hadError),其余继续——对齐 cat/grep 多文件语义。
func openInputs(env CmdEnv, files []string) (readers []namedReader, closers []io.Closer, hadError bool) {
	if len(files) == 0 {
		return []namedReader{{name: "-", r: env.Stdin}}, nil, false
	}
	for _, f := range files {
		if f == "-" {
			readers = append(readers, namedReader{name: "-", r: env.Stdin})
			continue
		}
		fh, err := openFile(env, f)
		if err != nil {
			fmt.Fprintf(env.Stderr, "%s: %v\n", f, err)
			hadError = true
			continue
		}
		closers = append(closers, fh)
		readers = append(readers, namedReader{name: f, r: fh})
	}
	return readers, closers, hadError
}

func closeAll(closers []io.Closer) {
	for _, c := range closers {
		_ = c.Close()
	}
}

// scanLines 逐行读 r(剥掉行尾 \n),周期 check ctx 取消(纯 CPU/IO 循环响应超时的关键)。
// 累计读取超 maxReadBytes 返回 errReadLimit。回调返回 false 提前停止。
func scanLines(ctx context.Context, r io.Reader, fn func(line string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // 容忍长行(8MB)
	var read int
	n := 0
	for sc.Scan() {
		if n%512 == 0 { // 每 512 行探一次 ctx,避免每行 select 开销
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		n++
		line := sc.Text()
		read += len(line) + 1
		if read > maxReadBytes {
			return errReadLimit
		}
		if !fn(line) {
			return nil
		}
	}
	return sc.Err()
}

// errReadLimit 单命令读取超上限。
var errReadLimit = fmt.Errorf("input exceeded %d bytes; narrow the range (head/tail/sed line window) before piping", maxReadBytes)

// ioErrExit 把命令执行中的 IO/ctx 错误翻译成 exit code 并写 stderr。
// ctx 取消/超时 → 124(由上层 classifyRunError 再确认 TimedOut);其余 → 1。
func ioErrExit(env CmdEnv, cmd string, err error) int {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return exitTimedOut
	}
	fmt.Fprintf(env.Stderr, "%s: %v\n", cmd, err)
	return exitGenericError
}
