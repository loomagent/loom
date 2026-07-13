package gobash

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"strings"
	"unicode"
)

// cmdPwd 打印逻辑工作目录。workspace 根映射成 "/"。
func cmdPwd(_ context.Context, env CmdEnv, _ []string) int {
	dir := env.Dir
	if strings.TrimSpace(dir) == "" {
		dir = "/"
	}
	io.WriteString(env.Stdout, dir+"\n")
	return exitOK
}

// cmdCat 拼接文件/stdin。支持 -n(行号)。
func cmdCat(ctx context.Context, env CmdEnv, args []string) int {
	set, _, pos := scanFlags(args, "", []string{"number"})
	number := anySet(set, "n", "number")
	readers, closers, hadErr := openInputs(env, pos)
	defer closeAll(closers)
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	lineNo := 0
	exit := exitOK
	if hadErr {
		exit = exitGenericError
	}
	for _, nr := range readers {
		err := scanLines(ctx, nr.r, func(line string) bool {
			if number {
				lineNo++
				w.WriteString(strings.Repeat(" ", max(0, 6-len(strconv.Itoa(lineNo)))))
				w.WriteString(strconv.Itoa(lineNo))
				w.WriteByte('\t')
			}
			w.WriteString(line)
			w.WriteByte('\n')
			return true
		})
		if err != nil {
			w.Flush()
			return ioErrExit(env, "cat", err)
		}
	}
	return exit
}

// cmdNl 给行编号(简化:对齐 nl -ba,所有行都编号)。
func cmdNl(ctx context.Context, env CmdEnv, args []string) int {
	_, _, pos := scanFlags(args, "b", nil)
	readers, closers, hadErr := openInputs(env, pos)
	defer closeAll(closers)
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	lineNo := 0
	for _, nr := range readers {
		err := scanLines(ctx, nr.r, func(line string) bool {
			lineNo++
			s := strconv.Itoa(lineNo)
			w.WriteString(strings.Repeat(" ", max(0, 6-len(s))))
			w.WriteString(s)
			w.WriteByte('\t')
			w.WriteString(line)
			w.WriteByte('\n')
			return true
		})
		if err != nil {
			w.Flush()
			return ioErrExit(env, "nl", err)
		}
	}
	if hadErr {
		return exitGenericError
	}
	return exitOK
}

// cmdHead 取前 N 行(默认 10)或前 N 字节(-c)。多文件加 ==> file <== 头。
func cmdHead(ctx context.Context, env CmdEnv, args []string) int {
	set, vals, pos := scanFlags(args, "nc", []string{"lines", "bytes"})
	return headTail(ctx, env, "head", set, vals, pos, true)
}

// cmdTail 取后 N 行(默认 10)或后 N 字节(-c)。-f 明确拒绝。
func cmdTail(ctx context.Context, env CmdEnv, args []string) int {
	set, vals, pos := scanFlags(args, "nc", []string{"lines", "bytes"})
	if anySet(set, "f", "follow") {
		io.WriteString(env.Stderr, "tail: -f (follow) 不支持(workspace 是静态只读快照)\n")
		return exitUsageError
	}
	return headTail(ctx, env, "tail", set, vals, pos, false)
}

func headTail(ctx context.Context, env CmdEnv, cmd string, set map[string]bool, vals map[string][]string, pos []string, isHead bool) int {
	byteMode := anySet(set, "c", "bytes")
	n := 10
	if v, ok := firstVal(vals, "c", "bytes", "n", "lines"); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || parsed < 0 {
			io.WriteString(env.Stderr, cmd+": 无效的数量: "+v+"\n")
			return exitUsageError
		}
		n = parsed
	} else if k, ok := numericShortFlag(set); ok {
		// GNU 简写:head -1 / tail -5 等同 -n 1 / -n 5。
		n = k
	}
	readers, closers, hadErr := openInputs(env, pos)
	defer closeAll(closers)
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	multi := len(readers) > 1
	for idx, nr := range readers {
		if multi {
			if idx > 0 {
				w.WriteString("\n")
			}
			w.WriteString("==> " + nr.name + " <==\n")
		}
		var err error
		if byteMode {
			err = emitBytes(ctx, nr.r, w, n, isHead)
		} else {
			err = emitLines(ctx, nr.r, w, n, isHead)
		}
		if err != nil {
			w.Flush()
			return ioErrExit(env, cmd, err)
		}
	}
	if hadErr {
		return exitGenericError
	}
	return exitOK
}

func emitLines(ctx context.Context, r io.Reader, w io.Writer, n int, isHead bool) error {
	if isHead {
		count := 0
		return scanLines(ctx, r, func(line string) bool {
			if count >= n {
				return false
			}
			count++
			io.WriteString(w, line+"\n")
			return true
		})
	}
	// tail:环形缓冲保留最后 n 行。
	if n == 0 {
		return drain(ctx, r)
	}
	ring := make([]string, 0, n)
	err := scanLines(ctx, r, func(line string) bool {
		if len(ring) < n {
			ring = append(ring, line)
		} else {
			copy(ring, ring[1:])
			ring[n-1] = line
		}
		return true
	})
	if err != nil {
		return err
	}
	for _, line := range ring {
		io.WriteString(w, line+"\n")
	}
	return nil
}

func emitBytes(ctx context.Context, r io.Reader, w io.Writer, n int, isHead bool) error {
	if isHead {
		_, err := io.CopyN(w, r, int64(n))
		if err == io.EOF {
			return nil
		}
		return err
	}
	// tail -c:读全量(受 maxReadBytes 限),取尾 n 字节。
	lr := io.LimitReader(r, maxReadBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return err
	}
	if len(data) > maxReadBytes {
		return errReadLimit
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n < len(data) {
		data = data[len(data)-n:]
	}
	_, err = w.Write(data)
	return err
}

func drain(ctx context.Context, r io.Reader) error {
	return scanLines(ctx, r, func(string) bool { return true })
}

// cmdWc 统计行/词/字节。-l -w -c;无 flag 时三者都出。多文件加 total 行。
func cmdWc(ctx context.Context, env CmdEnv, args []string) int {
	set, _, pos := scanFlags(args, "", nil)
	wantL := anySet(set, "l", "lines")
	wantW := anySet(set, "w", "words")
	wantC := anySet(set, "c", "bytes")
	if !wantL && !wantW && !wantC {
		wantL, wantW, wantC = true, true, true
	}
	readers, closers, hadErr := openInputs(env, pos)
	defer closeAll(closers)
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	var totL, totW, totC int
	emit := func(l, words, bytes int, name string) {
		var parts []string
		if wantL {
			parts = append(parts, pad(l))
		}
		if wantW {
			parts = append(parts, pad(words))
		}
		if wantC {
			parts = append(parts, pad(bytes))
		}
		line := strings.Join(parts, " ")
		if name != "-" && name != "" {
			line += " " + name
		}
		w.WriteString(line + "\n")
	}
	for _, nr := range readers {
		var l, words, bytes int
		err := scanLines(ctx, nr.r, func(line string) bool {
			l++
			bytes += len(line) + 1
			words += len(strings.FieldsFunc(line, unicode.IsSpace))
			return true
		})
		if err != nil {
			w.Flush()
			return ioErrExit(env, "wc", err)
		}
		totL += l
		totW += words
		totC += bytes
		emit(l, words, bytes, nr.name)
	}
	if len(readers) > 1 {
		emit(totL, totW, totC, "total")
	}
	if hadErr {
		return exitGenericError
	}
	return exitOK
}

func pad(n int) string {
	s := strconv.Itoa(n)
	if len(s) >= 7 {
		return s
	}
	return strings.Repeat(" ", 7-len(s)) + s
}
