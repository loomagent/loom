// Package workspacebash 提供受限的 workspace bash 工具:给 agent 一个「像翻 codebase 一样」
// 翻 workspace 的真 shell,但用两层护栏关住——
//  1. Go 侧:用 mvdan.cc/sh 把命令 parse 成 AST,强制二进制 allowlist + 拒绝危险构造(永远在场);
//  2. 执行侧:命令经 gobash 纯 Go 进程内 runner 执行,所有 FS 访问经 os.Root 封闭在
//     workspace 根内、只读(无 daemon、无 bubblewrap、跨平台)。
//
// react 与 pro_report 共用本包(Runner 实现是 gobash)。bash 对 LLM 恒只读(不变量):
// validator 把写重定向拦在入口给可读报错,命令自带的写路径由 os.Root 只读兜底。
package workspacebash

import (
	"fmt"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// DefaultAllowlist 是约定的 Tier-1 只读工作集,必须与 gobash 进程内 registry 完全一致
// (gobash init 期断言)。排除一切交互/TUI/AST/网络工具与纯写场景命令。
// 进程内化后砍掉 rg(grep+RE2 覆盖,方言一致)、fd(find 覆盖)。
var DefaultAllowlist = []string{
	"grep",              // 内容搜索(RE2)
	"jq",                // 查 *.jsonl
	"find", "ls", "pwd", // 列文件 / 定位目录
	"sed", "head", "tail", "nl", // 切片 / 行窗口 / 行号(citation locator)
	"wc", "sort", "uniq", "cut", // 计数 / 去重 / 字段
	"cat", "stat", "file", "tr", // 读 / 元数据 / 字符变换
	"xargs", // 组合(jq/find 输出列表喂给 cat/grep 的唯一通路,命令替换被禁)
}

// Validator 用 AST 校验命令。零值不可用,请用 NewValidator。
type Validator struct {
	allow map[string]struct{}
}

// NewValidator 用给定 allowlist 建校验器;传 nil 用 DefaultAllowlist。
func NewValidator(allowlist []string) *Validator {
	if allowlist == nil {
		allowlist = DefaultAllowlist
	}
	allow := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		if name = strings.TrimSpace(name); name != "" {
			allow[name] = struct{}{}
		}
	}
	return &Validator{allow: allow}
}

// Allowed 返回排序后的 allowlist,用于错误提示。
func (v *Validator) Allowed() []string {
	out := make([]string, 0, len(v.allow))
	for name := range v.allow {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Validate 解析命令并强制安全约束。通过返回 nil,否则返回可读的拒绝原因(直接喂回模型)。
//
// 拒绝项:
//   - 任何不在 allowlist 的二进制(含管道/&&/; 链里的每一段)
//   - 命令替换 $(...) / 反引号(CmdSubst)、进程替换 <(...)（ProcSubst)
//   - 写盘重定向(> file, >> file, < file)恒拒绝;但 >/dev/null、2>/dev/null 屏蔽输出放行
//   - 后台执行 &
//   - 前置环境赋值(FOO=bar cmd)、命令名非字面量(防变量拼命令名 / IFS 把戏)
//   - 带路径的命令名(/usr/bin/rg)—— 只准裸名
//
// 允许:管道 |、逻辑 && ||、顺序 ;、子shell (...)、glob、引用 —— 只要每段命令都在 allowlist。
func (v *Validator) Validate(command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("空命令")
	}
	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		return fmt.Errorf("命令解析失败(语法非法): %v", err)
	}

	var rejectErr error
	reject := func(format string, args ...any) bool {
		if rejectErr == nil {
			rejectErr = fmt.Errorf(format, args...)
		}
		return false // 停止继续往下走
	}

	syntax.Walk(file, func(node syntax.Node) bool {
		if rejectErr != nil {
			return false
		}
		switch n := node.(type) {
		case *syntax.CmdSubst:
			return reject("禁止命令替换 $(...) / 反引号")
		case *syntax.ProcSubst:
			return reject("禁止进程替换 <(...) / >(...)")
		case *syntax.Redirect:
			if n.Word == nil {
				return reject("禁止该重定向")
			}
			if target, ok := literalWord(n.Word); !ok || target != "/dev/null" {
				return reject("workspace 只读,禁止重定向到文件(只允许 >/dev/null、2>/dev/null 屏蔽输出)。产物由系统自动落盘,不需要手动保存")
			}
		case *syntax.Stmt:
			if n.Background {
				return reject("禁止后台执行 &")
			}
		case *syntax.CallExpr:
			if len(n.Assigns) > 0 {
				return reject("禁止前置环境赋值(FOO=bar cmd)")
			}
			if len(n.Args) == 0 {
				return true
			}
			name, ok := literalWord(n.Args[0])
			if !ok {
				return reject("命令名必须是字面量(禁止用变量/展开拼命令名)")
			}
			if strings.Contains(name, "/") {
				return reject("命令名只准用裸名,不准带路径: %q", name)
			}
			if _, allowed := v.allow[name]; !allowed {
				return reject("命令 %q 不在允许列表。可用: %s", name, strings.Join(v.Allowed(), " "))
			}
			// grep 用 Go RE2 正则:`|` 才是「或」,`\|` 是「字面竖线字符」。模型常带 GNU grep
			// BRE 习惯写 \| 当或用 → RE2 下会去找字面串永远搜不到、静默假阴性。一律拒绝,逼它改对。
			if name == "grep" {
				for _, arg := range n.Args[1:] {
					if val, ok := literalWord(arg); ok && strings.Contains(val, `\|`) {
						return reject(`grep 模式里出现 \| —— 本工具用 RE2 正则,\| 是「字面竖线」不是「或」,会搜不到。` +
							`多关键词请用 grep -e A -e B,或 grep "A|B";若真要匹配字面竖线字符,用 "[|]"`)
					}
				}
			}
		}
		return true
	})
	return rejectErr
}

// literalWord 在 Word 是纯字面量(可被单/双引号包裹)时返回其值。
// 只要含任何展开(变量、命令替换等)就返回 ok=false。
func literalWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}
