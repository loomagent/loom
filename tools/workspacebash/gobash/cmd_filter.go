package gobash

import (
	"bufio"
	"context"
	"io"
	"sort"
	"strconv"
	"strings"
)

// cmdCut 抽取字段(-f)或字符(-c)。-d 指定分隔符(默认 TAB)。LIST 支持 N、N-M、N-、-M、逗号组合。
func cmdCut(ctx context.Context, env CmdEnv, args []string) int {
	set, vals, pos := scanFlags(args, "dfc", []string{"delimiter", "fields", "characters"})
	delim := "\t"
	if v, ok := firstVal(vals, "d", "delimiter"); ok {
		delim = v
	}
	var listSpec string
	charMode := false
	if v, ok := firstVal(vals, "f", "fields"); ok {
		listSpec = v
	} else if v, ok := firstVal(vals, "c", "characters"); ok {
		listSpec, charMode = v, true
	} else {
		io.WriteString(env.Stderr, "cut: 需要 -f LIST 或 -c LIST\n")
		return exitUsageError
	}
	sel, err := parseRanges(listSpec)
	if err != nil {
		io.WriteString(env.Stderr, "cut: 无效的范围: "+listSpec+"\n")
		return exitUsageError
	}
	_ = set
	readers, closers, hadErr := openInputs(env, pos)
	defer closeAll(closers)
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	for _, nr := range readers {
		e := scanLines(ctx, nr.r, func(line string) bool {
			if charMode {
				w.WriteString(selectChars(line, sel))
			} else {
				w.WriteString(selectFields(line, delim, sel))
			}
			w.WriteByte('\n')
			return true
		})
		if e != nil {
			w.Flush()
			return ioErrExit(env, "cut", e)
		}
	}
	if hadErr {
		return exitGenericError
	}
	return exitOK
}

func selectFields(line, delim string, sel rangeSet) string {
	fields := strings.Split(line, delim)
	var out []string
	for i, f := range fields {
		if sel.has(i + 1) {
			out = append(out, f)
		}
	}
	return strings.Join(out, delim)
}

func selectChars(line string, sel rangeSet) string {
	runes := []rune(line)
	var b strings.Builder
	for i, r := range runes {
		if sel.has(i + 1) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// rangeSet 是 cut LIST 的 1-based 选择器。
type rangeSet struct {
	spans [][2]int // [lo, hi];hi==0 表示开放上界(N-)
}

func (rs rangeSet) has(i int) bool {
	for _, s := range rs.spans {
		if i >= s[0] && (s[1] == 0 || i <= s[1]) {
			return true
		}
	}
	return false
}

func parseRanges(spec string) (rangeSet, error) {
	var rs rangeSet
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if dash := strings.IndexByte(part, '-'); dash >= 0 {
			loStr, hiStr := part[:dash], part[dash+1:]
			lo, hi := 1, 0
			if loStr != "" {
				v, err := strconv.Atoi(loStr)
				if err != nil {
					return rs, err
				}
				lo = v
			}
			if hiStr != "" {
				v, err := strconv.Atoi(hiStr)
				if err != nil {
					return rs, err
				}
				hi = v
			}
			rs.spans = append(rs.spans, [2]int{lo, hi})
		} else {
			v, err := strconv.Atoi(part)
			if err != nil {
				return rs, err
			}
			rs.spans = append(rs.spans, [2]int{v, v})
		}
	}
	return rs, nil
}

// cmdSort 排序。-n 数值、-r 倒序、-u 去重、-k N 按字段、-t SEP 字段分隔符。全量加载(受读上限)。
func cmdSort(ctx context.Context, env CmdEnv, args []string) int {
	set, vals, pos := scanFlags(args, "kt", []string{"key", "field-separator"})
	numeric := anySet(set, "n", "numeric-sort")
	reverse := anySet(set, "r", "reverse")
	unique := anySet(set, "u", "unique")
	sep := "\t"
	if v, ok := firstVal(vals, "t", "field-separator"); ok {
		sep = v
	}
	keyField := 0
	if v, ok := firstVal(vals, "k", "key"); ok {
		// -k 可能是 "2" 或 "2,2";取首段字段号。
		head := v
		if c := strings.IndexAny(v, ",."); c >= 0 {
			head = v[:c]
		}
		if n, err := strconv.Atoi(head); err == nil {
			keyField = n
		}
	}
	readers, closers, hadErr := openInputs(env, pos)
	defer closeAll(closers)
	var lines []string
	for _, nr := range readers {
		e := scanLines(ctx, nr.r, func(line string) bool {
			lines = append(lines, line)
			return true
		})
		if e != nil {
			return ioErrExit(env, "sort", e)
		}
	}
	keyOf := func(s string) string {
		if keyField <= 0 {
			return s
		}
		f := strings.Split(s, sep)
		if keyField-1 < len(f) {
			return f[keyField-1]
		}
		return ""
	}
	sort.SliceStable(lines, func(i, j int) bool {
		ki, kj := keyOf(lines[i]), keyOf(lines[j])
		var less bool
		if numeric {
			fi, _ := strconv.ParseFloat(strings.TrimSpace(ki), 64)
			fj, _ := strconv.ParseFloat(strings.TrimSpace(kj), 64)
			less = fi < fj
		} else {
			less = ki < kj
		}
		return less
	})
	if reverse {
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
	}
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	var prev string
	first := true
	for _, line := range lines {
		if unique && !first && line == prev {
			continue
		}
		w.WriteString(line)
		w.WriteByte('\n')
		prev = line
		first = false
	}
	if hadErr {
		return exitGenericError
	}
	return exitOK
}

// cmdUniq 折叠相邻重复行。-c 计数、-d 仅重复、-u 仅唯一。
func cmdUniq(ctx context.Context, env CmdEnv, args []string) int {
	set, _, pos := scanFlags(args, "", nil)
	count := anySet(set, "c", "count")
	onlyDup := anySet(set, "d", "repeated")
	onlyUniq := anySet(set, "u", "unique")
	readers, closers, hadErr := openInputs(env, pos)
	defer closeAll(closers)
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	var cur string
	var n int
	have := false
	flush := func() {
		if !have {
			return
		}
		isDup := n > 1
		if onlyDup && !isDup {
			return
		}
		if onlyUniq && isDup {
			return
		}
		if count {
			w.WriteString(pad(n))
			w.WriteByte(' ')
		}
		w.WriteString(cur)
		w.WriteByte('\n')
	}
	for _, nr := range readers {
		e := scanLines(ctx, nr.r, func(line string) bool {
			if have && line == cur {
				n++
				return true
			}
			flush()
			cur, n, have = line, 1, true
			return true
		})
		if e != nil {
			w.Flush()
			return ioErrExit(env, "uniq", e)
		}
	}
	flush()
	if hadErr {
		return exitGenericError
	}
	return exitOK
}
