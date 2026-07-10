package workspace

import (
	"fmt"
	"path"
	"strings"
)

// ValidatePath 校验 workspace 内部路径合法性。
//
// 规则:
//   - 必须以 "/" 开头(绝对路径)
//   - 不允许 "." / ".." 段(防回溯)
//   - 不允许连续 "/"
//   - 单段不允许空
//   - 长度 ≤ 256 字符
//   - 允许的字符:字母/数字/下划线/连字符/点/斜杠/中文/常见 unicode
//     (粗略校验,黑名单 control chars / null bytes)
//
// 返回 normalized path(剥末尾 "/" 等)。校验失败返 ErrInvalidPath wrap 描述。
func ValidatePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidPath)
	}
	if len(p) > 256 {
		return "", fmt.Errorf("%w: too long (>256 chars)", ErrInvalidPath)
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: must start with '/' (absolute path)", ErrInvalidPath)
	}
	// 控制字符检测
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: contains control char", ErrInvalidPath)
		}
	}
	// 路径段检查
	segments := strings.Split(p, "/")
	// 第一段是空(因为以 "/" 开头),最后一段允许空(目录路径如 "/" 或 "/a/")
	for i, seg := range segments {
		if i == 0 {
			continue // 开头的 ""
		}
		if seg == "" {
			if i == len(segments)-1 {
				continue // 末尾的目录斜杠
			}
			return "", fmt.Errorf("%w: contains empty segment (consecutive '/')", ErrInvalidPath)
		}
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: contains '.' or '..' (no path traversal)", ErrInvalidPath)
		}
	}

	// path.Clean 标准化(剥末尾 "/" 但保留根 "/")
	cleaned := path.Clean(p)
	return cleaned, nil
}

// ValidateDirPath 同 ValidatePath,但保留末尾 "/" 语义(Ls 的入参用)。
// 根目录 "/" 是合法的。
func ValidateDirPath(p string) (string, error) {
	cleaned, err := ValidatePath(p)
	if err != nil {
		return "", err
	}
	return cleaned, nil
}

// isUnder 判断 p 是否在 dir 之下(直接子项,不递归)。
// dir="/" 时,p 形如 "/foo" 算子项(无中间 "/")。
// dir="/a" 时,p 形如 "/a/foo" 算子项。
func isUnder(dir, p string) bool {
	if dir == "/" {
		// 根目录子项 = 去掉开头 "/" 后不含 "/"
		rest := strings.TrimPrefix(p, "/")
		return rest != "" && !strings.Contains(rest, "/")
	}
	prefix := dir + "/"
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	rest := p[len(prefix):]
	return rest != "" && !strings.Contains(rest, "/")
}

// listVirtualDirs 给定一组文件 path,提取出位于 dir 下的"虚拟子目录"集合。
// 例:dir="/", files=["/a/x.md","/a/y.md","/b.md"] → 虚拟目录 ["/a"],文件 ["/b.md"]
//
// 返:
//   - dirs:虚拟子目录绝对路径(去重,sorted 由调用方处理)
//   - files:dir 下直接子文件(原样)
func listVirtualDirs(dir string, allPaths []string) (dirs []string, files []string) {
	dirSet := map[string]struct{}{}
	for _, p := range allPaths {
		if !strings.HasPrefix(p, dir) {
			continue
		}
		if dir != "/" && !strings.HasPrefix(p, dir+"/") {
			continue
		}
		// p 是 dir 下文件吗?
		if isUnder(dir, p) {
			files = append(files, p)
			continue
		}
		// 不是直接子项,提取下一级目录段
		var rest string
		if dir == "/" {
			rest = strings.TrimPrefix(p, "/")
		} else {
			rest = strings.TrimPrefix(p, dir+"/")
		}
		if rest == "" {
			continue
		}
		segs := strings.SplitN(rest, "/", 2)
		if len(segs) < 2 {
			continue // p 本身就是 dir 下子文件,已经在上面处理
		}
		// 构造虚拟目录绝对路径
		var subDir string
		if dir == "/" {
			subDir = "/" + segs[0]
		} else {
			subDir = dir + "/" + segs[0]
		}
		dirSet[subDir] = struct{}{}
	}
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	return dirs, files
}
