// Package workspace 提供 agent 用的虚拟文件系统(workspace)能力。
//
// 设计目的:支持 2026 范式的两阶段 long-horizon task —
//
//	Phase 1(Research):agent 用 web_search/web_reader 等 web tool 抓取知识,
//	                    通过 write_file 把整理后的知识沉淀成本地文件
//	Phase 2(Execution):agent 用 read_file / edit_file 在本地文件上做闭环 reasoning,
//	                    不再调网络工具,确保 determinism + speed + consistency
//
// 设计要点:
//   - Backend 是可插拔的(InMemoryBackend 用于内存场景;应用可实现持久化后端)
//   - 跨 turn 共享 workspace:同一 conversation 内,turn N+1 能看到 turn N 写的文件
//   - "Read-before-Write":修改已存在文件前必须先 read,防 LLM 盲改(参考 Claude Code)
//   - 文件 = 一系列 write_file / edit_file 操作的 fold(每个 path 取最后一次写)
//
// 工具集(4 个,精简版):
//   - ls(path)
//   - read_file(path, offset?, limit?)
//   - write_file(path, content)
//   - edit_file(path, old_string, new_string, replace_all?)
//
// 不提供 grep / glob / execute — 第一版业务场景 LLM 自己 read + reason 够用,
// sandbox 留作长期演进(那时再加 run_code 单一工具,不直接暴露 shell)。
package workspace

import (
	"context"
	"errors"
	"time"
)

// FileInfo 文件元信息,Ls / Glob 返回。
type FileInfo struct {
	Path       string    // 绝对路径,如 "/research/openai_o3.md"
	IsDir      bool      // 当前 workspace 用 path 前缀派生"目录",IsDir=true 表示一个虚拟目录
	Size       int64     // bytes(directory 为 0)
	ModifiedAt time.Time // 最近一次 Write / Edit 的时间
}

// Backend workspace 的存储后端接口(可插拔)。
//
// 实现要求:
//   - 路径都是绝对路径("/" 开头),由 Validate 统一校验
//   - Read 不存在的 path 返 ErrFileNotFound
//   - Write 创建/覆盖,不要求 path 父目录存在(目录是虚拟的)
//   - Edit:OldString 在文件中出现 0 次 → ErrEditNoMatch;>1 次且 ReplaceAll=false → ErrEditAmbiguous
//   - "Read-before-Write" 由 Backend 自身维护(Read 标记 path 已读,Write/Edit 已存在文件前必须已读)
type Backend interface {
	// Ls 列出 path 下直接子项(不递归)。
	// path="/" 列根目录;其它路径返回该"虚拟目录"下子项。
	Ls(ctx context.Context, path string) ([]FileInfo, error)

	// Read 读文件内容,支持按行分页(offset 1-based,limit=0 = 全部)。
	// 不带行号包装(行号由 read_file 工具 wrapper 加),raw content。
	Read(ctx context.Context, path string, offset, limit int) (string, error)

	// Write 创建或覆盖文件。
	// 若 path 已存在,要求调用方在本 Backend 生命周期内先调过 Read(path),否则返 ErrMustReadFirst。
	Write(ctx context.Context, path, content string) error

	// Edit 字符串替换。
	// 要求 path 已存在 + 已调过 Read(path)。
	// OldString 在文件出现 0 次返 ErrEditNoMatch;>1 次且 replaceAll=false 返 ErrEditAmbiguous。
	// 返 replaced = 实际替换次数(replaceAll=false 时永远是 1)。
	Edit(ctx context.Context, path, oldString, newString string, replaceAll bool) (replaced int, err error)
}

// Backend 错误集(用 errors.Is 判断)。
var (
	ErrFileNotFound   = errors.New("workspace: file not found")
	ErrMustReadFirst  = errors.New("workspace: must read file before write/edit")
	ErrEditNoMatch    = errors.New("workspace: old_string not found in file")
	ErrEditAmbiguous  = errors.New("workspace: old_string appears multiple times (use replace_all)")
	ErrInvalidPath    = errors.New("workspace: invalid path")
	ErrFileTooLarge   = errors.New("workspace: file too large")
	ErrTooManyFiles   = errors.New("workspace: workspace file count limit exceeded")
	ErrSameOldAndNew  = errors.New("workspace: old_string and new_string must differ")
	ErrEmptyOldString = errors.New("workspace: old_string must not be empty")
)

// Limits 各 Backend 实现共享的约束常量(单文件大小 / 工作区总文件数等)。
//
// 保守值,够 long-horizon task 用,防 LLM 失控写满。
const (
	// MaxFileBytes 单文件最大字节数。
	// 1 MB 够长 markdown / JSON 数据集用,超了 LLM context 也吃不下。
	MaxFileBytes = 1 << 20

	// MaxWorkspaceFiles 工作区最大文件数(去重后)。
	// 跨 turn 累积,200 个文件够任意 long-horizon task。
	MaxWorkspaceFiles = 200

	// DefaultReadLimit read_file 默认行数上限(不传 limit 时用)。
	// 跟 Claude Code 一致,防一次性把超长文件灌进 LLM context。
	DefaultReadLimit = 2000
)
