package loom

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
)

// Role 消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 一条上下文消息(纯文本)。
// 不支持多模态;后续扩展会以追加字段方式做(不改现有 Content 语义)。
type Message struct {
	Role Role

	// Content 主体内容。
	// 对 reasoning 模型的 assistant 消息,这是去掉 reasoning 后的最终答案文本。
	// role=assistant 且仅触发 tool_calls 没有文本输出时可为空。
	Content string

	// ReasoningContent 推理过程。
	// 仅 assistant 消息可能填充;user/system/tool 应留空。
	// 不支持推理的 provider 收到此字段时应静默忽略。
	ReasoningContent string

	// ToolCalls 仅 role=assistant 携带:本轮 LLM 发起的工具调用列表。
	// 用于把上一轮的 tool_calls 拼回对话历史,让 LLM 看到完整的"我调过哪些工具"。
	ToolCalls []ToolCall

	// Name 可选:消息发送者名字(部分 provider 支持,用于多用户上下文区分)。
	Name string

	// ToolCallID 仅 role=tool 时填,关联触发本结果的工具调用 id(对应某条 ToolCall.ID)。
	ToolCallID string
}

// FinishReason 模型停止的原因。
type FinishReason string

const (
	FinishReasonStop          FinishReason = "stop"           // 自然停止
	FinishReasonLength        FinishReason = "length"         // 撞 max_tokens
	FinishReasonContentFilter FinishReason = "content_filter" // 被内容审核截断
	FinishReasonToolCalls     FinishReason = "tool_calls"     // 模型请求调工具
	FinishReasonError         FinishReason = "error"          // provider 异常归因
)

// Usage Token 用量。各字段含义按下:
//   - PromptTokens:    输入 token
//   - CompletionTokens:输出 token (含 reasoning)
//   - CachedTokens:    输入命中 prompt cache 的 token 数(provider 支持时)
//   - ReasoningTokens: 输出中属于 reasoning 部分的 token 数(reasoning 模型)
//   - TotalTokens:     Prompt + Completion
type Usage struct {
	PromptTokens     uint64
	CompletionTokens uint64
	CachedTokens     uint64
	ReasoningTokens  uint64
	TotalTokens      uint64
}

// ReasoningEffort 推理预算等级。具体含义由 provider 翻译;
// 各模型支持的档位子集由 ModelCapabilities.ReasoningEfforts 声明,
// ResolveReasoning 负责能力校验(deepseek: high/max;doubao: low/medium/high)。
// "minimal=不思考"类档位不建模 — 不思考用 ReasoningModeDisabled 表达。
type ReasoningEffort string

const (
	ReasoningEffortDefault ReasoningEffort = ""       // provider 默认
	ReasoningEffortLow     ReasoningEffort = "low"    // 较低推理预算
	ReasoningEffortMedium  ReasoningEffort = "medium" // 中等推理预算
	ReasoningEffortHigh    ReasoningEffort = "high"   // 较高推理预算
	ReasoningEffortMax     ReasoningEffort = "max"    // 最高推理预算
)

// ReasoningMode 推理开关。
//
// 必传:零值("")表示调用方未声明,provider 构造请求时直接报错。
// 不存在"跟随服务端默认"的选项——供应商可能悄悄翻转默认行为
// (deepseek-v4 系列服务端默认开 thinking,曾导致标题总结的 max_tokens 被
// reasoning 耗尽、content 为空静默截断),每个调用点必须显式回答"这次要不要推理"。
type ReasoningMode string

const (
	ReasoningModeEnabled  ReasoningMode = "enabled"  // 显式开启推理
	ReasoningModeDisabled ReasoningMode = "disabled" // 显式关闭推理
)

// Reasoning 推理控制。Mode 必传,见 ReasoningMode。
type Reasoning struct {
	Mode   ReasoningMode
	Effort ReasoningEffort
}

// ResponseFormat 输出格式控制。
type ResponseFormat string

const (
	ResponseFormatDefault    ResponseFormat = ""            // provider 默认(通常 text)
	ResponseFormatText       ResponseFormat = "text"        // 普通文本
	ResponseFormatJSONObject ResponseFormat = "json_object" // 强制 JSON 输出(provider 支持时)
)

// StructuredOutputMode 描述结构化输出能力/请求档位。
//
// 用在两处,取值集合略有差异:
//   - ModelCapabilities.StructuredOutput(能力声明):None=明确声明不支持,
//     ""=未声明(跳过能力校验、纯透传);
//   - ChatRequest.StructuredOutput.Mode(请求):JSONObject/JSONSchema,
//     ""(Unsupported)表示不要结构化输出。
type StructuredOutputMode string

const (
	StructuredOutputUnsupported StructuredOutputMode = ""            // 请求:不要结构化输出 / 能力:未声明
	StructuredOutputNone        StructuredOutputMode = "none"        // 能力专用:明确声明不支持
	StructuredOutputJSONObject  StructuredOutputMode = "json_object" // 仅能要求 JSON object
	StructuredOutputJSONSchema  StructuredOutputMode = "json_schema" // 可传 JSON Schema
)

// ReasoningSupport 模型推理支持形态(四态)。
// 四态合一建模而非拆 supports + default_on 两个 bool,
// 避免出现「不支持推理但默认开启」的非法组合。
type ReasoningSupport string

const (
	ReasoningSupportNone                 ReasoningSupport = "none"                   // 不支持推理
	ReasoningSupportAlwaysOn             ReasoningSupport = "always_on"              // 永远推理,不可关
	ReasoningSupportToggleableDefaultOn  ReasoningSupport = "toggleable_default_on"  // 可开关,服务端默认开
	ReasoningSupportToggleableDefaultOff ReasoningSupport = "toggleable_default_off" // 可开关,服务端默认关
)

// ModelCapabilities 是模型在初始化时声明的能力。
//
// 能力由调用方或 modelfactory 按实际模型配置填充,provider 包不写死
// 任何默认值。裸构造(不传 Capabilities)时一律是零值"未声明",
// 各能力校验对未声明跳过、纯透传。
type ModelCapabilities struct {
	StructuredOutput StructuredOutputMode

	// Reasoning 推理支持形态。零值("")表示未声明 — ResolveReasoning 对未声明
	// 能力只做 Mode 必传校验、跳过能力交叉校验(纯透传)。
	Reasoning ReasoningSupport
	// ReasoningEfforts 支持的推理强度档位,空 = 不支持调档。
	ReasoningEfforts []ReasoningEffort
	// MaxOutputTokens 单次输出 token 上限,0 = 未知。
	MaxOutputTokens uint64
	// MaxContextTokens 上下文窗口 token 上限,0 = 未知。
	// 仅用于组装期预算/裁剪,不参与构造期硬拦截。
	MaxContextTokens uint64
}

// ReasoningSend 推理参数的发送决策(ResolveReasoning 的输出,provider 无关)。
type ReasoningSend string

const (
	ReasoningSendOmit     ReasoningSend = "omit"     // 不发送推理开关参数
	ReasoningSendEnabled  ReasoningSend = "enabled"  // 显式发送"开启推理"
	ReasoningSendDisabled ReasoningSend = "disabled" // 显式发送"关闭推理"
)

// ResolvedReasoning 推理声明按模型能力解析后的结果。
// provider 在 buildRequest 里把它翻译成各自的请求参数
// (deepseek → thinking + reasoning_effort;ark → Thinking 字段)。
type ResolvedReasoning struct {
	Send ReasoningSend
	// Effort 仅 Send=enabled 时可能非空。
	Effort ReasoningEffort
}

// CheckRequestAgainstCapabilities 防御性校验:请求用到模型声明不支持的能力时
// 在构造阶段直接报错,绝不发出去(「声明与使用矛盾要暴露,不静默降级/被吞」)。
//
// 覆盖结构化输出与输出长度;推理由 ResolveReasoning 单独校验。
// 能力未声明(对应字段为零值)时跳过对应校验、纯透传。
//
// 有意不校验输入是否超 max_context_tokens(别加)。2026-06 实测三家供应商
// (deepseek/ark/openrouter)超窗均在生成前返回 400 — 不计费、报文带精确
// token 数,且各 provider 的 ErrorClassifier 都把 400 归为 Permanent 不重试,
// 错误即时上抛,供应商报错本身就是"提前报错"。而本地拦截必有误杀:各家校验
// 语义不一致(deepseek/openrouter 把 messages+completion 加总校验;ark 只看
// 输入,输入+max_tokens 加总超窗实测合法可生成),本地统一规则不可能同时对齐,
// 且本地估算永远不如供应商 tokenizer 准。max_context_tokens 的消费场景是
// 预算类(组装期裁剪/多轮循环用 Usage.PromptTokens 观测),不是构造期报错。
//
// 注意与 ChatStructured 的分工:ChatStructured 按声明能力主动降级
// (json_schema → json_object → prompt 兜底),它构造的请求永远不会越过声明;
// 本校验拦的是绕过 ChatStructured 直接手写 ChatRequest 的越界使用。
func CheckRequestAgainstCapabilities(caps ModelCapabilities, req ChatRequest) error {
	if req.StructuredOutput != nil {
		switch req.StructuredOutput.Mode {
		case StructuredOutputJSONSchema:
			switch caps.StructuredOutput {
			case StructuredOutputJSONSchema:
				// 支持
			case StructuredOutputUnsupported:
				// 能力未声明,透传
			case StructuredOutputNone, StructuredOutputJSONObject:
				return fmt.Errorf("loom: 模型声明 structured_output=%q,不支持 json_schema", caps.StructuredOutput)
			default:
				return fmt.Errorf("loom: 未知 structured output 能力 %q", caps.StructuredOutput)
			}
		case StructuredOutputJSONObject:
			switch caps.StructuredOutput {
			case StructuredOutputJSONObject, StructuredOutputJSONSchema:
				// 支持
			case StructuredOutputUnsupported:
				// 能力未声明,透传
			case StructuredOutputNone:
				return fmt.Errorf("loom: 模型声明 structured_output=none,不支持 json_object")
			default:
				return fmt.Errorf("loom: 未知 structured output 能力 %q", caps.StructuredOutput)
			}
		case StructuredOutputUnsupported:
			// 请求不要结构化输出,无需校验
		case StructuredOutputNone:
			return fmt.Errorf("loom: StructuredOutput.Mode 不允许取 %q(none 是能力声明专用值)", req.StructuredOutput.Mode)
		default:
			return fmt.Errorf("loom: 未知 StructuredOutput.Mode %q", req.StructuredOutput.Mode)
		}
	} else if req.ResponseFormat == ResponseFormatJSONObject {
		switch caps.StructuredOutput {
		case StructuredOutputJSONObject, StructuredOutputJSONSchema:
			// 支持
		case StructuredOutputUnsupported:
			// 能力未声明,透传
		case StructuredOutputNone:
			return fmt.Errorf("loom: 模型声明 structured_output=none,不支持 response_format=json_object")
		default:
			return fmt.Errorf("loom: 未知 structured output 能力 %q", caps.StructuredOutput)
		}
	}

	if req.MaxTokens != nil && *req.MaxTokens > 0 &&
		caps.MaxOutputTokens > 0 && uint64(*req.MaxTokens) > caps.MaxOutputTokens {
		return fmt.Errorf("loom: MaxTokens=%d 超过模型声明的输出上限 %d", *req.MaxTokens, caps.MaxOutputTokens)
	}
	return nil
}

// ResolveReasoning 按模型能力解析调用方的推理声明。矩阵全集见
// reasoning_test.go 的 TestResolveReasoningMatrix;要点:
//   - Mode 必传:零值直接报错,这是"每个调用点显式决策"的强制点;
//   - 声明与能力矛盾(none×enabled / always_on×disabled / 档位越界)报错而非静默忽略;
//   - 能力未声明(caps.Reasoning=="")只做必传校验,纯透传。
func ResolveReasoning(caps ModelCapabilities, r Reasoning) (ResolvedReasoning, error) {
	switch r.Mode {
	case ReasoningModeEnabled, ReasoningModeDisabled:
		// 合法,继续
	case "":
		return ResolvedReasoning{}, fmt.Errorf("loom: Reasoning.Mode 必传(enabled/disabled),不允许依赖供应商服务端默认行为")
	default:
		return ResolvedReasoning{}, fmt.Errorf("loom: 未知 Reasoning.Mode %q", r.Mode)
	}

	switch r.Effort {
	case ReasoningEffortDefault, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortMax:
		// 合法,继续
	default:
		return ResolvedReasoning{}, fmt.Errorf("loom: 未知 Reasoning.Effort %q", r.Effort)
	}

	switch r.Mode {
	case ReasoningModeEnabled:
		switch caps.Reasoning {
		case ReasoningSupportNone:
			return ResolvedReasoning{}, fmt.Errorf("loom: 模型不支持推理(reasoning_support=none),不能要求 enabled")
		case ReasoningSupportAlwaysOn,
			ReasoningSupportToggleableDefaultOn,
			ReasoningSupportToggleableDefaultOff,
			"":
			// 显式发送 enabled(always_on 时无害且更显式;能力未声明时透传)
		default:
			return ResolvedReasoning{}, fmt.Errorf("loom: 未知 reasoning_support %q", caps.Reasoning)
		}
		if r.Effort != ReasoningEffortDefault &&
			len(caps.ReasoningEfforts) > 0 &&
			!slices.Contains(caps.ReasoningEfforts, r.Effort) {
			return ResolvedReasoning{}, fmt.Errorf("loom: 模型不支持推理强度档位 %q(支持: %v)", r.Effort, caps.ReasoningEfforts)
		}
		return ResolvedReasoning{Send: ReasoningSendEnabled, Effort: r.Effort}, nil

	case ReasoningModeDisabled:
		if r.Effort != ReasoningEffortDefault {
			return ResolvedReasoning{}, fmt.Errorf("loom: Reasoning.Mode=disabled 与 Effort=%q 矛盾(关闭推理不应指定推理强度)", r.Effort)
		}
		switch caps.Reasoning {
		case ReasoningSupportNone:
			// 模型本无推理,不发参数
			return ResolvedReasoning{Send: ReasoningSendOmit}, nil
		case ReasoningSupportAlwaysOn:
			return ResolvedReasoning{}, fmt.Errorf("loom: 该模型推理不可关闭(reasoning_support=always_on),不能要求 disabled")
		case ReasoningSupportToggleableDefaultOn,
			ReasoningSupportToggleableDefaultOff,
			"":
			return ResolvedReasoning{Send: ReasoningSendDisabled}, nil
		default:
			return ResolvedReasoning{}, fmt.Errorf("loom: 未知 reasoning_support %q", caps.Reasoning)
		}

	default:
		// 不可达:Mode 已在开头校验
		return ResolvedReasoning{}, fmt.Errorf("loom: 未知 Reasoning.Mode %q", r.Mode)
	}
}

// StructuredOutput 是一次调用的结构化输出约束。
//
// 没有 Strict 字段是有意的:strict("供应商硬保证输出合规")永远是调用方想要的,
// 不存在"希望输出可以违反 schema"的场景,所以 provider 在 json_schema 模式下
// 固定发 strict=true,不做成开关。2026-06 实测 deepseek/ark/openrouter 对
// strict 参数都静默接受(无校验管线、无行为差异),不会因此报错;将来接入
// OpenAI 式实现 strict 的供应商时,不满足 strict 子集规则的 schema(可选字段、
// 缺 additionalProperties:false)会在请求时 400(Permanent 不重试),改 schema 即可。
type StructuredOutput struct {
	Mode        StructuredOutputMode
	Name        string
	Description string
	Schema      *jsonschema.Schema
}

// ChatRequest 一次 LLM 调用的参数。
// 必填:Messages。其它字段为零值时使用 provider 默认。
type ChatRequest struct {
	Messages []Message

	// Tools 本次调用允许 LLM 使用的工具列表;nil 或空 = 不暴露任何工具。
	// 工具实现侧不在此字段内 — 这里只传 ToolInfo 元数据给 LLM。
	Tools []*ToolInfo

	// ToolChoice 工具调用策略;nil = provider 默认(有 Tools 时等价于 Auto)。
	ToolChoice *ToolChoice

	Temperature *float64
	TopP        *float64
	MaxTokens   *int

	// Stop 自定义停止序列。
	// 长度 1 时翻译成 provider 的单 stop;>1 时翻译成 stop 数组。
	Stop []string

	Reasoning      Reasoning
	ResponseFormat ResponseFormat

	// StructuredOutput 优先于 ResponseFormat。支持 json_schema 的 provider 会把
	// Schema 原样传给模型;仅支持 json_object 的 provider 会退化成 JSON object。
	StructuredOutput *StructuredOutput
}

// ChatResponse 同步调用的完整结果。
type ChatResponse struct {
	Content          string
	ReasoningContent string

	// ToolCalls LLM 本轮发起的工具调用(可能为空)。
	// agent 拿到后用 Tool.Invoke 执行,把结果作为 role=tool 消息塞回历史进入下一轮。
	ToolCalls []ToolCall

	FinishReason FinishReason
	Usage        Usage

	// Model provider 实际使用的 model id(审计 / observability;
	// 可能与请求时声明的 model 不同,如 provider 做了别名解析)。
	Model string
}

// Chunk 流式调用的一个增量片段。
// 任一 *Delta 字段可能为空字符串(本帧没有该通道的内容);
// FinishReason / Usage 通常仅最后一帧填充。
type Chunk struct {
	ContentDelta          string
	ReasoningContentDelta string

	// ToolCallDeltas 流式工具调用增量;按 Index 累加拼成完整 ToolCall。
	// 调用方负责按 Index 维护累积态(或用 loom 提供的拼装 helper,待后续添加)。
	ToolCallDeltas []ToolCallDelta

	FinishReason FinishReason // 仅最后一帧填,中间帧为 ""
	Usage        *Usage       // 仅有 Usage 信息的帧(通常最后一帧)填,否则 nil
	Model        string
}

// Stream 流式调用的句柄。
// 调用方典型用法:
//
//	s, err := model.Stream(ctx, req)
//	if err != nil { ... }
//	defer s.Close()
//	for {
//	    ch, err := s.Recv()
//	    if errors.Is(err, io.EOF) { break }
//	    if err != nil { return err }
//	    if ch == nil { continue } // provider 偶尔发空帧,跳过
//	    // 处理 ch
//	}
type Stream interface {
	// Recv 拿下一个 chunk。流自然结束时返回 io.EOF。
	// 可能返回 (nil, nil) 表示一个无信息的空帧(调用方应 continue)。
	Recv() (*Chunk, error)
	// Close 释放底层连接,幂等。
	Close() error
}

// ChatModel 一个具体模型实例(provider × model 组合)。
// 实现见 providers/* 子包。
type ChatModel interface {
	// Name 返回模型可读标识,形如 "deepseek/deepseek-v4-flash"。
	// 用于 log / observability,不参与请求路由。
	Name() string

	// Capabilities 返回模型初始化时声明的能力,供调用侧决定是否启用结构化输出等特性。
	Capabilities() ModelCapabilities

	// Chat 同步调用,阻塞直到完整结果返回。
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)

	// Stream 流式调用。调用方必须 Close 返回的 Stream。
	Stream(ctx context.Context, req ChatRequest) (Stream, error)
}
