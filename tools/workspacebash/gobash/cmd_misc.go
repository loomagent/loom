package gobash

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"strings"
)

// cmdStat 打印文件元数据。支持 -c FMT 子集(%n 名、%s 字节、%Y mtime epoch、%F 类型);默认打印 "name size mtime"。
func cmdStat(_ context.Context, env CmdEnv, args []string) int {
	_, vals, pos := scanFlags(args, "c", []string{"format", "printf"})
	format, hasFmt := firstVal(vals, "c", "format", "printf")
	if len(pos) == 0 {
		io.WriteString(env.Stderr, "stat: 缺少文件\n")
		return exitUsageError
	}
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	exit := exitOK
	for _, p := range pos {
		info, err := env.Root.Stat(toRootRel(env.Dir, p))
		if err != nil {
			io.WriteString(env.Stderr, "stat: "+p+": "+err.Error()+"\n")
			exit = exitGenericError
			continue
		}
		ftype := "regular file"
		if info.IsDir() {
			ftype = "directory"
		}
		if hasFmt {
			r := strings.NewReplacer(
				"%n", p,
				"%s", strconv.FormatInt(info.Size(), 10),
				"%Y", strconv.FormatInt(info.ModTime().Unix(), 10),
				"%F", ftype,
			)
			w.WriteString(r.Replace(format))
			w.WriteByte('\n')
		} else {
			w.WriteString(p + " " + strconv.FormatInt(info.Size(), 10) + " " + info.ModTime().Format("2006-01-02 15:04:05") + "\n")
		}
	}
	return exit
}

// cmdFile 极简文件类型判别:读首块,含 NUL 或大量非文本字节 → data,否则 text。
func cmdFile(_ context.Context, env CmdEnv, args []string) int {
	_, _, pos := scanFlags(args, "", nil)
	if len(pos) == 0 {
		io.WriteString(env.Stderr, "file: 缺少文件\n")
		return exitUsageError
	}
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	exit := exitOK
	for _, p := range pos {
		fh, err := openFile(env, p)
		if err != nil {
			io.WriteString(env.Stderr, "file: "+p+": "+err.Error()+"\n")
			exit = exitGenericError
			continue
		}
		buf := make([]byte, 8192)
		n, _ := io.ReadFull(fh, buf)
		fh.Close()
		w.WriteString(p + ": " + classifyBytes(buf[:n]) + "\n")
	}
	return exit
}

func classifyBytes(b []byte) string {
	if len(b) == 0 {
		return "empty"
	}
	nonText := 0
	for _, c := range b {
		if c == 0 {
			return "data"
		}
		if c < 0x09 || (c > 0x0d && c < 0x20) {
			nonText++
		}
	}
	if nonText*100/len(b) > 5 {
		return "data"
	}
	return "ASCII text"
}

// cmdTr 字符变换(只读 stdin → stdout)。支持 SET1 SET2 翻译、-d 删除、-s 压缩重复。
// SET 支持字面字符与范围(a-z);不支持 [:class:] 等高级语法。
func cmdTr(ctx context.Context, env CmdEnv, args []string) int {
	set, _, pos := scanFlags(args, "", nil)
	del := anySet(set, "d", "delete")
	squeeze := anySet(set, "s", "squeeze-repeats")

	var set1, set2 []rune
	if len(pos) >= 1 {
		set1 = expandSet(pos[0])
	}
	if len(pos) >= 2 {
		set2 = expandSet(pos[1])
	}
	if len(set1) == 0 {
		io.WriteString(env.Stderr, "tr: 缺少字符集\n")
		return exitUsageError
	}

	// 翻译表。
	trans := map[rune]rune{}
	if !del && len(set2) > 0 {
		for i, r := range set1 {
			if i < len(set2) {
				trans[r] = set2[i]
			} else {
				trans[r] = set2[len(set2)-1] // 不足则补最后一个
			}
		}
	}
	delSet := map[rune]bool{}
	if del {
		for _, r := range set1 {
			delSet[r] = true
		}
	}
	sqSet := map[rune]bool{}
	if squeeze {
		src := set2
		if del || len(set2) == 0 {
			src = set1
		}
		for _, r := range src {
			sqSet[r] = true
		}
	}

	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	var last rune = -1
	err := scanRunes(ctx, env.Stdin, func(r rune) {
		if del && delSet[r] {
			return
		}
		if t, ok := trans[r]; ok {
			r = t
		}
		if squeeze && sqSet[r] && r == last {
			return
		}
		w.WriteRune(r)
		last = r
	})
	if err != nil {
		w.Flush()
		return ioErrExit(env, "tr", err)
	}
	return exitOK
}

// expandSet 把 "a-z0-9_" 展开成 rune 列表(支持 X-Y 范围)。
func expandSet(s string) []rune {
	runes := []rune(s)
	var out []rune
	for i := 0; i < len(runes); i++ {
		if i+2 < len(runes) && runes[i+1] == '-' {
			lo, hi := runes[i], runes[i+2]
			if lo <= hi {
				for c := lo; c <= hi; c++ {
					out = append(out, c)
				}
				i += 2
				continue
			}
		}
		out = append(out, runes[i])
	}
	return out
}

// scanRunes 逐 rune 读 r,周期 check ctx,受 maxReadBytes 约束。
func scanRunes(ctx context.Context, r io.Reader, fn func(rune)) error {
	br := bufio.NewReader(r)
	var read int
	n := 0
	for {
		if n%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		n++
		ch, sz, err := br.ReadRune()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		read += sz
		if read > maxReadBytes {
			return errReadLimit
		}
		fn(ch)
	}
}
