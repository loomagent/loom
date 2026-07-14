package workspacebash

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/loomagent/loom"
)

// ToolName 是暴露给 agent 的 bash 工具名。
const ToolName = "bash"

// Runner 执行一条已通过校验的命令。生产路径是 gobash 进程内 runner。
type Runner interface {
	Run(ctx context.Context, command string) (Result, error)
}

type ToolOptions struct {
	// Runner 必填。
	Runner Runner
	// Validator 为 nil 时用 NewValidator(nil)。
	Validator *Validator
	// Description 必填,各执行器自带的工具说明(react/pro_report 文案不同)。
	Description string
	// Parameters 为 nil 时用默认的单 command 字段 schema。
	Parameters *jsonschema.Schema
	// ErrorOnTruncated 为 true 时,输出被截断直接返回 error,逼模型换更窄的命令
	// 重试(react 语义);为 false 时返回部分输出并附截断提示(pro_report 语义)。
	ErrorOnTruncated bool
	// MaxStdout/MaxStderr 仅用于 ErrorOnTruncated 模式的错误文案。
	MaxStdout uint64
	MaxStderr uint64
}

type commandRequest struct {
	Command string `json:"command" jsonschema:"Shell command to execute." validate:"min=1,notblank"`
}

// DefaultParameters 返回默认的工具入参 schema(单 command 字段)。
func DefaultParameters(commandDescription string) *jsonschema.Schema {
	params := loom.MustSchemaFor[commandRequest]()
	if description := strings.TrimSpace(commandDescription); description != "" {
		params.Properties["command"].Description = description
	}
	return params
}

// NewTool 把「校验 + 执行 + 结果格式化」包成一个 loom 工具。
func NewTool(opts ToolOptions) loom.Tool {
	validator := opts.Validator
	if validator == nil {
		validator = NewValidator(nil)
	}
	params := opts.Parameters
	if params == nil {
		params = DefaultParameters("要执行的 shell 命令,例如 `grep -rn \"keyword\" /raw` 或 `jq -r .url /sources.jsonl | head -20`。")
	}
	contract := loom.MustToolContract[commandRequest](ToolName, loom.WithArgumentSchema(params))
	return loom.NewTypedTool(contract, opts.Description, func(ctx context.Context, args commandRequest) (string, error) {
		command := strings.TrimSpace(args.Command)
		if err := validator.Validate(command); err != nil {
			// 校验拒绝当作工具错误返回,模型据此自我纠正。
			return "", fmt.Errorf("命令被拒: %v", err)
		}
		result, err := opts.Runner.Run(ctx, command)
		if err != nil {
			return "", err
		}
		if opts.ErrorOnTruncated {
			if result.StdoutTruncated {
				return "", fmt.Errorf("command output exceeded %d bytes; retry with narrower commands such as grep -n, head, tail, sed -n, or jq filters", opts.MaxStdout)
			}
			if result.StderrTruncated {
				return "", fmt.Errorf("command stderr exceeded %d bytes; retry with narrower commands", opts.MaxStderr)
			}
		}
		return formatResult(result), nil
	})
}

// formatResult 把执行结果格式化成喂回 LLM 的文本。
// 截断提示只在结果仍然返回时出现(ErrorOnTruncated 模式截断早已转成 error)。
func formatResult(r Result) string {
	var b strings.Builder
	if r.Stdout != "" {
		b.WriteString(r.Stdout)
	}
	if r.Stderr != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[stderr]\n")
		b.WriteString(r.Stderr)
	}
	if r.ExitCode != 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[exit code: %d]", r.ExitCode)
	}
	if r.StdoutTruncated || r.StderrTruncated {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[输出已截断,超过上限;请用更窄的范围重查(grep -n 定位 + sed -n 行窗口)]")
	}
	if r.TimedOut {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[命令超时被终止,以上为部分输出;请缩小范围或拆分命令后重试]")
	}
	if b.Len() == 0 {
		return "[无输出]"
	}
	return b.String()
}
