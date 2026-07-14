package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/loomagent/loom"
)

// 工具名常量。
const (
	ToolNameLs        = "ls"
	ToolNameReadFile  = "read_file"
	ToolNameWriteFile = "write_file"
	ToolNameEditFile  = "edit_file"
)

const (
	tracerName        = "github.com/loomagent/loom/tools/workspace"
	attrWorkspacePath = "workspace.path"
)

func workspaceTracer() trace.Tracer { return otel.Tracer(tracerName) }

type lsRequest struct {
	Path string `json:"path" jsonschema:"Absolute directory path to list (must start with '/'). Use '/' to list workspace root." validate:"min=1,notblank" example:"/"`
}

type readFileRequest struct {
	Path   string `json:"path" jsonschema:"Absolute path of the file to read." validate:"min=1,notblank" example:"/notes.md"`
	Offset int    `json:"offset,omitempty" jsonschema:"1-based starting line number. Zero uses the default of 1." validate:"omitempty,min=0"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Max lines to read. Default 2000. Zero uses the default." validate:"omitempty,min=0"`
}

type writeFileRequest struct {
	Path    string `json:"path" jsonschema:"Absolute path of the file to write (creates or overwrites)." validate:"min=1,notblank" example:"/notes.md"`
	Content string `json:"content" jsonschema:"Full file content. UTF-8 text, max 1 MB." example:"hello"`
}

type editFileRequest struct {
	Path       string `json:"path" jsonschema:"Absolute path of the file to edit." validate:"min=1,notblank" example:"/notes.md"`
	OldString  string `json:"old_string" jsonschema:"Exact string to replace (whitespace-sensitive). Must be non-empty." validate:"min=1" example:"hello"`
	NewString  string `json:"new_string" jsonschema:"Replacement string. Must differ from old_string. Empty string means delete." example:"hello world"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"If true, replace all occurrences. If false (default), error when old_string appears multiple times."`
}

// NewLs 构造 ls 工具。
//
// LLM 用 ls(path) 列目录;"目录"是虚拟的,从已有文件 path 派生。
func NewLs(b Backend) loom.Tool {
	contract := loom.MustToolContract[lsRequest](ToolNameLs)
	desc := "List files and virtual directories under the given path. " +
		"Path must be absolute. Returns entries sorted with directories first."
	return loom.NewTypedTool(contract, desc, func(ctx context.Context, in lsRequest) (string, error) {
		ctx, span := workspaceTracer().Start(ctx, "workspace.ls")
		defer span.End()
		span.SetAttributes(attribute.String(attrWorkspacePath, in.Path))
		infos, err := b.Ls(ctx, in.Path)
		if err != nil {
			return failTool(span, err)
		}
		span.SetStatus(codes.Ok, "")
		return marshalLsResult(infos), nil
	})
}

// NewReadFile 构造 read_file 工具。
//
// 默认带 cat -n 风格行号(`     1→content` 格式),跟 Claude Code 一致。
// offset 1-based,limit=0 走 DefaultReadLimit 兜底。
func NewReadFile(b Backend) loom.Tool {
	contract := loom.MustToolContract[readFileRequest](ToolNameReadFile)
	desc := "Read a file from the workspace. Returns content with cat -n style line numbers " +
		"(format: '     N→content'). Default reads up to 2000 lines starting from line 1; " +
		"use offset/limit to paginate large files. ALWAYS call read_file before write_file or " +
		"edit_file on an existing path."
	return loom.NewTypedTool(contract, desc, func(ctx context.Context, in readFileRequest) (string, error) {
		ctx, span := workspaceTracer().Start(ctx, "workspace.read_file")
		defer span.End()
		span.SetAttributes(
			attribute.String(attrWorkspacePath, in.Path),
			attribute.Int64("workspace.offset", int64(in.Offset)),
			attribute.Int64("workspace.limit", int64(in.Limit)),
		)
		content, err := b.Read(ctx, in.Path, in.Offset, in.Limit)
		if err != nil {
			return failTool(span, err)
		}
		span.SetStatus(codes.Ok, "")
		return formatWithLineNumbers(content, in.Offset), nil
	})
}

// NewWriteFile 构造 write_file 工具。
//
// 创建或覆盖 path。Backend 强制对已存在文件 read-before-write。
// Output 只返元数据 {path, bytes},content 已在 loom 内核写入的 tool_call.arguments,
// 不需要在 result 里重复(hydrate 时直接读 arguments)。
func NewWriteFile(b Backend) loom.Tool {
	contract := loom.MustToolContract[writeFileRequest](ToolNameWriteFile)
	desc := "Write a file to the workspace, creating or overwriting it. " +
		"For an existing path you MUST call read_file first (the tool will error otherwise) " +
		"so you don't blindly overwrite content."
	return loom.NewTypedTool(contract, desc, func(ctx context.Context, in writeFileRequest) (string, error) {
		ctx, span := workspaceTracer().Start(ctx, "workspace.write_file")
		defer span.End()
		span.SetAttributes(
			attribute.String(attrWorkspacePath, in.Path),
			attribute.Int64("workspace.bytes", int64(len(in.Content))),
		)
		if err := b.Write(ctx, in.Path, in.Content); err != nil {
			return failTool(span, err)
		}
		span.SetStatus(codes.Ok, "")
		out, _ := json.Marshal(map[string]any{
			"path":  in.Path,
			"bytes": len(in.Content),
		})
		return string(out), nil
	})
}

// NewEditFile 构造 edit_file 工具(精确字符串替换,参考 Claude Code)。
//
// 要求 path 已存在 + 调用前已 read_file。
// OldString 在文件中出现 0 次 → 报错;>1 次且 replace_all=false → 报错(LLM 必须显式提供更长上下文或开 replace_all)。
func NewEditFile(b Backend) loom.Tool {
	contract := loom.MustToolContract[editFileRequest](ToolNameEditFile)
	desc := "Perform an exact string replacement on an existing file. " +
		"You MUST call read_file on the path first. " +
		"If old_string is not unique, either provide more surrounding context to make it unique, " +
		"or set replace_all=true."
	return loom.NewTypedTool(contract, desc, func(ctx context.Context, in editFileRequest) (string, error) {
		ctx, span := workspaceTracer().Start(ctx, "workspace.edit_file")
		defer span.End()
		span.SetAttributes(
			attribute.String(attrWorkspacePath, in.Path),
			attribute.Bool("workspace.replace_all", in.ReplaceAll),
		)
		replaced, err := b.Edit(ctx, in.Path, in.OldString, in.NewString, in.ReplaceAll)
		if err != nil {
			return failTool(span, err)
		}
		span.SetAttributes(attribute.Int64("workspace.replaced", int64(replaced)))
		span.SetStatus(codes.Ok, "")
		out, _ := json.Marshal(map[string]any{
			"path":     in.Path,
			"replaced": replaced,
		})
		return string(out), nil
	})
}

// RegisterAll 把 4 个工具一次性注册到 ToolRegistry,executor handler 调用便利。
func RegisterAll(reg *loom.ToolRegistry, b Backend) error {
	tools := []loom.Tool{
		NewLs(b),
		NewReadFile(b),
		NewWriteFile(b),
		NewEditFile(b),
	}
	for _, t := range tools {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// failTool 统一失败路径:记 span 错误 + 返 (空 output, err)。
func failTool(span trace.Span, err error) (string, error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	// 已知 Backend 错误返人类可读 message 给 LLM,LLM 据此自纠
	if errors.Is(err, ErrFileNotFound) ||
		errors.Is(err, ErrMustReadFirst) ||
		errors.Is(err, ErrEditNoMatch) ||
		errors.Is(err, ErrEditAmbiguous) ||
		errors.Is(err, ErrInvalidPath) ||
		errors.Is(err, ErrFileTooLarge) ||
		errors.Is(err, ErrTooManyFiles) ||
		errors.Is(err, ErrSameOldAndNew) ||
		errors.Is(err, ErrEmptyOldString) {
		return "", err
	}
	return "", err
}

// marshalLsResult Ls 返回值 → JSON。
//
// 输出格式:
//
//	{
//	  "entries": [
//	    {"path": "/research", "is_dir": true},
//	    {"path": "/research/o3.md", "is_dir": false, "size": 1234, "modified_at": "..."}
//	  ]
//	}
func marshalLsResult(infos []FileInfo) string {
	entries := make([]map[string]any, 0, len(infos))
	for _, fi := range infos {
		e := map[string]any{
			"path":   fi.Path,
			"is_dir": fi.IsDir,
		}
		if !fi.IsDir {
			e["size"] = fi.Size
			e["modified_at"] = fi.ModifiedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		entries = append(entries, e)
	}
	out, _ := json.Marshal(map[string]any{"entries": entries})
	return string(out)
}

// formatWithLineNumbers cat -n 风格:5 位右对齐行号 + "→" 分隔 + content。
//
// 例:
//
//	1→# Research Notes
//	2→
//	3→## OpenAI o3
//
// startLine 是首行的实际行号(对应 read 的 offset)。
func formatWithLineNumbers(content string, offset int) string {
	if offset < 1 {
		offset = 1
	}
	lines := strings.Split(content, "\n")
	var sb strings.Builder
	for i, line := range lines {
		lineNo := offset + i
		fmt.Fprintf(&sb, "%6s→%s", strconv.Itoa(lineNo), line)
		if i < len(lines)-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}
