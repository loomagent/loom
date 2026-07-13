package gobash

import (
	"bufio"
	"context"
	"io"
	"regexp"
	"strconv"
)

// sedWindow 匹配受支持的唯一脚本形态:行窗口打印 `N p` / `Np` / `N,Mp` / `N,$p`。
var sedWindow = regexp.MustCompile(`^\s*(\d+)\s*(?:,\s*(\d+|\$)\s*)?p\s*$`)

// cmdSed 只实现 `sed -n 'N,Mp'` 行窗口(含 `Np`、`N,$p`)——模型读 raw/SRC-N.md 片段的唯一用法。
// 其余 sed 能力(s///、-i 原地写、d 删除等)一律拒绝并给可读提示。
func cmdSed(ctx context.Context, env CmdEnv, args []string) int {
	set, _, pos := scanFlags(args, "", nil)
	if !anySet(set, "n", "quiet", "silent") {
		io.WriteString(env.Stderr, "sed: 只支持 -n 'N,Mp' 行窗口模式(只读 workspace 不支持 s///、-i 等写/变换脚本)\n")
		return exitUsageError
	}
	if len(pos) == 0 {
		io.WriteString(env.Stderr, "sed: 缺少脚本(用法: sed -n 'N,Mp' 文件)\n")
		return exitUsageError
	}
	m := sedWindow.FindStringSubmatch(pos[0])
	if m == nil {
		io.WriteString(env.Stderr, "sed: 只支持行窗口脚本 'N,Mp'(如 sed -n '1,120p');其它脚本在只读 workspace 不可用\n")
		return exitUsageError
	}
	lo, _ := strconv.Atoi(m[1])
	hi := lo
	switch {
	case m[2] == "$":
		hi = -1 // 到文件末尾
	case m[2] != "":
		hi, _ = strconv.Atoi(m[2])
	}

	readers, closers, hadErr := openInputs(env, pos[1:])
	defer closeAll(closers)
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	for _, nr := range readers {
		lineNo := 0
		e := scanLines(ctx, nr.r, func(line string) bool {
			lineNo++
			if lineNo < lo {
				return true
			}
			if hi >= 0 && lineNo > hi {
				return false // 过了窗口上界,本文件可停
			}
			w.WriteString(line)
			w.WriteByte('\n')
			return true
		})
		if e != nil {
			w.Flush()
			return ioErrExit(env, "sed", e)
		}
	}
	if hadErr {
		return exitGenericError
	}
	return exitOK
}
