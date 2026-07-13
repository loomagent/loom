package gobash

import (
	"bufio"
	"context"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// cmdLs 列目录。-l 详细、-a 含隐藏、-1 每行一个(默认即如此,管道场景)、-R 递归。
func cmdLs(ctx context.Context, env CmdEnv, args []string) int {
	set, _, pos := scanFlags(args, "", nil)
	long := anySet(set, "l")
	all := anySet(set, "a", "all")
	recursive := anySet(set, "R", "recursive")
	if len(pos) == 0 {
		pos = []string{"."}
	}
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	exit := exitOK
	multi := len(pos) > 1 || recursive
	for _, p := range pos {
		if err := ctx.Err(); err != nil {
			return ioErrExit(env, "ls", err)
		}
		rel := toRootRel(env.Dir, p)
		info, err := env.Root.Stat(rel)
		if err != nil {
			io.WriteString(env.Stderr, "ls: "+p+": "+err.Error()+"\n")
			exit = exitGenericError
			continue
		}
		if !info.IsDir() {
			writeLsEntry(w, info, long)
			continue
		}
		if listDir(ctx, env, w, p, rel, long, all, recursive, multi) != exitOK {
			exit = exitGenericError
		}
	}
	return exit
}

func listDir(ctx context.Context, env CmdEnv, w *bufio.Writer, display, rel string, long, all, recursive, header bool) int {
	entries, err := fs.ReadDir(env.Root.FS(), rel)
	if err != nil {
		io.WriteString(env.Stderr, "ls: "+display+": "+err.Error()+"\n")
		return exitGenericError
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if header {
		w.WriteString(display + ":\n")
	}
	var subdirs []string
	for _, e := range entries {
		if !all && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if long {
			if info, ierr := e.Info(); ierr == nil {
				writeLsEntry(w, info, true)
			} else {
				w.WriteString(e.Name() + "\n")
			}
		} else {
			w.WriteString(e.Name() + "\n")
		}
		if recursive && e.IsDir() {
			subdirs = append(subdirs, e.Name())
		}
	}
	if recursive {
		for _, sd := range subdirs {
			w.WriteByte('\n')
			listDir(ctx, env, w, path.Join(display, sd), path.Join(rel, sd), long, all, recursive, true)
		}
	}
	return exitOK
}

func writeLsEntry(w *bufio.Writer, info fs.FileInfo, long bool) {
	if !long {
		w.WriteString(info.Name() + "\n")
		return
	}
	w.WriteString(info.Mode().String())
	w.WriteByte(' ')
	w.WriteString(pad(int(info.Size())))
	w.WriteByte(' ')
	w.WriteString(info.ModTime().Format("2006-01-02 15:04"))
	w.WriteByte(' ')
	w.WriteString(info.Name())
	w.WriteByte('\n')
}

// cmdFind 遍历目录树。-name GLOB(匹配 basename)、-type f/d、-maxdepth N。-exec 明确拒绝(用 xargs)。
func cmdFind(ctx context.Context, env CmdEnv, args []string) int {
	set, vals, pos := scanFlags(args, "", []string{"name", "type", "maxdepth"})
	if anySet(set, "exec", "delete") {
		io.WriteString(env.Stderr, "find: -exec/-delete 不支持(只读;用 xargs 组合只读命令)\n")
		return exitUsageError
	}
	namePat, hasName := firstVal(vals, "name")
	typeFilter, hasType := firstVal(vals, "type")
	maxDepth := -1
	if v, ok := firstVal(vals, "maxdepth"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			maxDepth = n
		}
	}
	// 起始路径 = 不带前导 '-' 的位置参数;默认 "."。
	var starts []string
	for _, p := range pos {
		if !strings.HasPrefix(p, "-") {
			starts = append(starts, p)
		}
	}
	if len(starts) == 0 {
		starts = []string{"."}
	}

	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	exit := exitOK
	for _, start := range starts {
		startRel := toRootRel(env.Dir, start)
		err := fs.WalkDir(env.Root.FS(), startRel, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return nil // 跳过不可读项,继续
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if maxDepth >= 0 {
				depth := 0
				if rest := strings.TrimPrefix(p, startRel); rest != "" {
					depth = strings.Count(strings.Trim(rest, "/"), "/") + 1
				}
				if depth > maxDepth {
					if d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
			}
			if hasType {
				if typeFilter == "f" && d.IsDir() {
					return nil
				}
				if typeFilter == "d" && !d.IsDir() {
					return nil
				}
			}
			if hasName {
				ok, _ := path.Match(namePat, d.Name())
				if !ok {
					return nil
				}
			}
			// 显示路径:用用户给的 start 前缀替换 walk 的 startRel 前缀。
			display := start
			if rest := strings.TrimPrefix(p, startRel); rest != "" {
				display = strings.TrimRight(start, "/") + rest
			}
			w.WriteString(display + "\n")
			return nil
		})
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return ioErrExit(env, "find", cerr)
			}
			io.WriteString(env.Stderr, "find: "+start+": "+err.Error()+"\n")
			exit = exitGenericError
		}
	}
	return exit
}
