package loom

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// ToolInfo 一个工具的元数据,供 LLM 决定何时 / 如何调用。
//
// Name 需要在工具集合内唯一;Description 应清楚说明"何时用 / 为什么用",
// 必要时给少样本示例 — 这是影响 LLM 调用准确率的关键。
//
// Parameters 是 JSON Schema (draft 2020-12 或 draft-07)。
// nil 表示无入参;空 *jsonschema.Schema{} 等价于"接受任意 JSON"。
type ToolInfo struct {
	Name            string
	Description     string
	Parameters      *jsonschema.Schema
	RequiresNetwork bool
}

// Tool 一个可调用工具。
//
// 典型 react 循环用法:
//  1. 把工具元数据(Tool.Info)通过 ChatRequest.Tools 暴露给 LLM
//  2. LLM 返回 ChatResponse.ToolCalls (流式则是 Chunk.ToolCallDeltas 拼装而成)
//  3. 业务方 ToolRegistry.Lookup(call.Name) 找到 Tool,调 Tool.Invoke(call.Arguments)
//  4. 把返回的 JSON 字符串包成 Message{Role: RoleTool, ToolCallID: call.ID, Content: result}
//     拼回历史,进入下一轮
//
// loom 不内置"自动跑这个循环"的高层节点;agent 业务方手写循环,框架只提供原子接口。
type Tool interface {
	// Info 返回工具元数据。
	// 同一 Tool 实例多次调用应返回逻辑等价的结果(允许 ctx 影响,但不宜频繁变化)。
	Info(ctx context.Context) (*ToolInfo, error)

	// Invoke 执行工具。
	//
	//   argumentsJSON: LLM 给出的工具入参,JSON 字符串(可能不合法 — 由 Tool 自行 validate)。
	//   返回值: 喂回给 LLM 的 tool result,JSON 字符串(任意结构,LLM 会按文本理解)。
	//
	// 错误处理约定:
	//   - 工具内部失败时返回 err,由调用方决定是否把 err.Error() 当作 result 反喂 LLM
	//     (让 LLM 自行决定换工具 / 重试 / 放弃)。
	//   - Invoke 自身不做"把错误转 result"的兜底 — 那是 agent 层的职责。
	Invoke(ctx context.Context, argumentsJSON string) (string, error)
}

type invokeFunc func(ctx context.Context, argumentsJSON string) (string, error)

type ToolOption func(*ToolInfo)

func WithRequiresNetwork() ToolOption {
	return func(info *ToolInfo) {
		info.RequiresNetwork = true
	}
}

func newTool(name, description string, params *jsonschema.Schema, fn invokeFunc, opts ...ToolOption) Tool {
	info := &ToolInfo{Name: name, Description: description, Parameters: params}
	for _, opt := range opts {
		if opt != nil {
			opt(info)
		}
	}
	return &funcTool{
		info: info,
		fn:   fn,
	}
}

type funcTool struct {
	info *ToolInfo
	fn   invokeFunc
}

func (t *funcTool) Info(context.Context) (*ToolInfo, error) {
	return t.info, nil
}

func (t *funcTool) Invoke(ctx context.Context, argumentsJSON string) (string, error) {
	return t.fn(ctx, argumentsJSON)
}

// ToolCall LLM 发起的一次工具调用(非流式响应或流式拼装完成后)。
type ToolCall struct {
	// ID provider 分配的调用标识,用于配对 tool result(作为 role=tool 消息的 ToolCallID)。
	ID string
	// Name 被调用的工具名。
	Name string
	// Arguments 工具入参,JSON 字符串(LLM 输出,不保证合法)。
	Arguments string
}

// ToolCallDelta 流式工具调用增量。
//
// 单个 ToolCall 可能跨多个 Chunk 拼出:
//   - 首帧通常带 Index/ID/Name + 第一段 Arguments
//   - 后续帧只带 Index + Arguments 增量(append 到累积值)
//
// 多个并发 ToolCall 通过 Index 区分(同 Index 的多帧增量同属一个 ToolCall)。
type ToolCallDelta struct {
	Index     int
	ID        string // 仅首帧填,后续帧空字符串
	Name      string // 仅首帧填,后续帧空字符串
	Arguments string // 本帧的增量片段
}

// ToolChoiceMode 工具选择策略。
type ToolChoiceMode string

const (
	// ToolChoiceAuto LLM 自决是否调用工具(等价于 ChatRequest.ToolChoice 为 nil)。
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceNone 强制 LLM 不调用任何工具。
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceRequired 强制 LLM 至少调用一个工具。
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceSpecific 强制调用 ToolChoice.Name 指定的工具。
	ToolChoiceSpecific ToolChoiceMode = "specific"
)

// ToolChoice 工具选择控制。
// 通过 ChatRequest.ToolChoice 传入;nil 表示 provider 默认(通常 = Auto)。
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name 仅 Mode=ToolChoiceSpecific 时使用,指定必须调用的工具名。
	Name string
}

// ToolRegistry 按名字管理一组工具,常作为 ChatRequest.Tools 列表的来源。
//
// 同时也方便 agent 在拿到 ChatResponse.ToolCalls 后用 Lookup 找到对应 Tool 执行。
type ToolRegistry struct {
	tools map[string]Tool
	order []string
}

// NewToolRegistry 构造注册表。可变参一次性 register;失败 panic(启动期暴露 bug)。
func NewToolRegistry(tools ...Tool) *ToolRegistry {
	r := &ToolRegistry{tools: map[string]Tool{}}
	for _, t := range tools {
		if err := r.Register(t); err != nil {
			panic(fmt.Sprintf("loom: NewToolRegistry: %v", err))
		}
	}
	return r
}

// Subset 按名字选子集,返回新的 ToolRegistry(不修改原 registry)。
// 任一 name 未注册返错(启动期 / 调用前发现配置错误)。
//
// 典型用法:executor 内声明全集,handler 按 chat_mode.Tools 选子集。
func (r *ToolRegistry) Subset(names []string) (*ToolRegistry, error) {
	out := NewToolRegistry()
	for _, name := range names {
		t, ok := r.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("loom: tool %q 不在 registry 中", name)
		}
		if err := out.Register(t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Register 注册一个工具;重名报错。
// 工具的 Name 通过 Info 取(此处会调一次 Info(context.Background()))。
func (r *ToolRegistry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("loom: tool 不能为 nil")
	}
	info, err := t.Info(context.Background())
	if err != nil {
		return fmt.Errorf("loom: Tool.Info 失败: %w", err)
	}
	if info == nil || info.Name == "" {
		return fmt.Errorf("loom: 工具 Name 不能为空")
	}
	if _, exists := r.tools[info.Name]; exists {
		return fmt.Errorf("loom: 工具 %q 已注册", info.Name)
	}
	r.tools[info.Name] = t
	r.order = append(r.order, info.Name)
	return nil
}

// Lookup 查找指定名字的工具。
func (r *ToolRegistry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// InfoList 收集所有工具的 ToolInfo,按注册顺序返回。
// 任一 Tool.Info 出错都中断并返回错误。
func (r *ToolRegistry) InfoList(ctx context.Context) ([]*ToolInfo, error) {
	out := make([]*ToolInfo, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		if t == nil {
			continue // 不变量上不会发生:order 和 tools 同步维护
		}
		info, err := t.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("loom: 工具 %q Info: %w", name, err)
		}
		out = append(out, info)
	}
	return out, nil
}
