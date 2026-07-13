package gobash

import (
	"bufio"
	"context"
	"io"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
)

// cmdGrep 在文件/stdin 里按 RE2 正则搜行。支持 -r -n -i -v -c -l -e -F -h -q、多 -e、glob 路径。
// 不支持 -A/-B/-C 上下文行、-P(PCRE)。RE2 方言:A|B 是「或」、\| 是字面竖线(与 rg 一致)。
func cmdGrep(ctx context.Context, env CmdEnv, args []string) int {
	set, vals, pos := scanFlags(args, "e", []string{"regexp"})
	recursive := anySet(set, "r", "R", "recursive")
	withLineNo := anySet(set, "n", "line-number")
	ignoreCase := anySet(set, "i", "ignore-case")
	invert := anySet(set, "v", "invert-match")
	countOnly := anySet(set, "c", "count")
	listFiles := anySet(set, "l", "files-with-matches")
	fixed := anySet(set, "F", "fixed-strings")
	noFilename := anySet(set, "h", "no-filename")
	quiet := anySet(set, "q", "silent", "s")

	patterns := append([]string{}, vals["e"]...)
	patterns = append(patterns, vals["regexp"]...)
	files := pos
	if len(patterns) == 0 {
		if len(pos) == 0 {
			io.WriteString(env.Stderr, "grep: 缺少检索模式\n")
			return exitUsageError
		}
		patterns = []string{pos[0]}
		files = pos[1:]
	}

	matcher, err := buildMatcher(patterns, fixed, ignoreCase)
	if err != nil {
		io.WriteString(env.Stderr, "grep: 无效的正则: "+err.Error()+"\n")
		return exitUsageError
	}

	// 收集待扫描的 (展示名, reader 工厂)。递归目录用 os.Root 遍历。
	type target struct {
		name string
		open func() (io.ReadCloser, error)
	}
	var targets []target
	hadError := false
	if len(files) == 0 {
		targets = append(targets, target{name: "-", open: func() (io.ReadCloser, error) {
			return io.NopCloser(env.Stdin), nil
		}})
	} else {
		for _, f := range files {
			rel := toRootRel(env.Dir, f)
			info, statErr := env.Root.Stat(rel)
			if statErr != nil {
				io.WriteString(env.Stderr, "grep: "+f+": "+statErr.Error()+"\n")
				hadError = true
				continue
			}
			if info.IsDir() {
				if !recursive {
					io.WriteString(env.Stderr, "grep: "+f+": 是目录(用 -r 递归)\n")
					hadError = true
					continue
				}
				walkErr := fs.WalkDir(env.Root.FS(), rel, func(p string, d fs.DirEntry, e error) error {
					if e != nil {
						return nil
					}
					if d.IsDir() {
						return nil
					}
					pp := p
					targets = append(targets, target{name: pp, open: func() (io.ReadCloser, error) {
						return env.Root.Open(pp)
					}})
					return nil
				})
				if walkErr != nil {
					hadError = true
				}
			} else {
				ff := f
				rr := rel
				targets = append(targets, target{name: ff, open: func() (io.ReadCloser, error) {
					return env.Root.Open(rr)
				}})
			}
		}
	}

	showName := !noFilename && (recursive || len(targets) > 1)
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	anyMatch := false

	for _, t := range targets {
		rc, openErr := t.open()
		if openErr != nil {
			io.WriteString(env.Stderr, "grep: "+t.name+": "+openErr.Error()+"\n")
			hadError = true
			continue
		}
		fileMatches := 0
		lineNo := 0
		scanErr := scanLines(ctx, rc, func(line string) bool {
			lineNo++
			hit := matcher(line)
			if invert {
				hit = !hit
			}
			if !hit {
				return true
			}
			anyMatch = true
			fileMatches++
			if quiet {
				return false // 静默:命中即停
			}
			if countOnly || listFiles {
				return true
			}
			if showName {
				w.WriteString(t.name)
				w.WriteByte(':')
			}
			if withLineNo {
				w.WriteString(strconv.Itoa(lineNo))
				w.WriteByte(':')
			}
			w.WriteString(line)
			w.WriteByte('\n')
			return true
		})
		rc.Close()
		if scanErr != nil {
			w.Flush()
			return ioErrExit(env, "grep", scanErr)
		}
		if quiet && anyMatch {
			return exitOK
		}
		if listFiles && fileMatches > 0 {
			w.WriteString(t.name)
			w.WriteByte('\n')
		}
		if countOnly {
			if showName {
				w.WriteString(t.name)
				w.WriteByte(':')
			}
			w.WriteString(strconv.Itoa(fileMatches))
			w.WriteByte('\n')
		}
	}

	if quiet {
		if anyMatch {
			return exitOK
		}
		return exitGenericError
	}
	if hadError {
		return 2
	}
	if anyMatch {
		return exitOK
	}
	return exitGenericError // grep 约定:无命中 exit 1
}

// buildMatcher 把多模式编译成单个匹配函数。-F 用子串包含,否则 RE2 正则(多模式合成 (p1)|(p2))。
func buildMatcher(patterns []string, fixed, ignoreCase bool) (func(string) bool, error) {
	if fixed {
		pats := patterns
		if ignoreCase {
			lowered := make([]string, len(pats))
			for i, p := range pats {
				lowered[i] = strings.ToLower(p)
			}
			pats = lowered
		}
		return func(line string) bool {
			s := line
			if ignoreCase {
				s = strings.ToLower(s)
			}
			for _, p := range pats {
				if strings.Contains(s, p) {
					return true
				}
			}
			return false
		}, nil
	}
	quoted := make([]string, len(patterns))
	for i, p := range patterns {
		quoted[i] = "(?:" + p + ")"
	}
	expr := strings.Join(quoted, "|")
	if ignoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	return re.MatchString, nil
}
