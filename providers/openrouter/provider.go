// Package openrouter 实现 loom.ChatModel,底层走 OpenRouter 的 OpenAI 兼容
// chat completions API(github.com/meguminnnnnnnnn/go-openai)。
//
// 与原生 OpenAI 协议的差异点(也是单独成包而非复用 deepseek/goseek 的原因):
//   - 推理参数:OpenRouter 统一用请求级 "reasoning" 对象
//     ({"enabled": bool, "effort": "low|medium|high"}),经 SetExtraFields 注入;
//   - 推理输出:响应/流式 delta 的 message 用 "reasoning" 字段(非 deepseek 的
//     "reasoning_content"),从 ExtraFields 提取。
//
// 用法:
//
//	model, err := openrouter.New(openrouter.Config{
//	    APIKey:    os.Getenv("OPENROUTER_API_KEY"),
//	    ModelName: "x-ai/grok-4.3",
//	})
package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	goOpenai "github.com/meguminnnnnnnnn/go-openai"

	"github.com/loomagent/loom"
)

// DefaultBaseURL OpenRouter API 入口。
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// Config OpenRouter provider 构造参数。
type Config struct {
	// APIKey 必填。
	APIKey string
	// ModelName 必填,OpenRouter 模型标识,如 "x-ai/grok-4.3"。
	ModelName string
	// BaseURL 可空 — 不设时用 DefaultBaseURL。
	BaseURL string

	// Retry 控制 retry 策略;nil 走 loom.DefaultRetryConfig()。
	Retry *loom.RetryConfig

	// Capabilities 模型能力,由调用方或 modelfactory 按实际模型配置填充。
	// nil = 零值"未声明"(能力校验跳过、纯透传)。
	Capabilities *loom.ModelCapabilities
}

// Model 一个 OpenRouter 模型实例,实现 loom.ChatModel。
type Model struct {
	client       *goOpenai.Client
	name         string
	retryCfg     *loom.RetryConfig
	capabilities loom.ModelCapabilities
}

var _ loom.ChatModel = (*Model)(nil)

// New 构造 Model。
func New(cfg Config) (*Model, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("loom/openrouter: APIKey 不能为空")
	}
	if strings.TrimSpace(cfg.ModelName) == "" {
		return nil, fmt.Errorf("loom/openrouter: ModelName 不能为空")
	}

	clientCfg := goOpenai.DefaultConfig(cfg.APIKey)
	clientCfg.BaseURL = DefaultBaseURL
	if cfg.BaseURL != "" {
		clientCfg.BaseURL = cfg.BaseURL
	}

	retryCfg := cfg.Retry
	if retryCfg == nil {
		retryCfg = loom.DefaultRetryConfig()
	}
	// 能力由调用方或 modelfactory 传入,这里不写死模型默认值。
	// 裸构造(nil)= 零值"未声明",各能力校验跳过、纯透传。
	capabilities := loom.ModelCapabilities{}
	if cfg.Capabilities != nil {
		capabilities = *cfg.Capabilities
	}
	return &Model{
		client:       goOpenai.NewClientWithConfig(clientCfg),
		name:         cfg.ModelName,
		retryCfg:     retryCfg,
		capabilities: capabilities,
	}, nil
}

// Name 返回 "openrouter/<model>" 形式标识。
func (m *Model) Name() string {
	return "openrouter/" + m.name
}

// Capabilities 返回初始化时声明的模型能力。
func (m *Model) Capabilities() loom.ModelCapabilities {
	return m.capabilities
}

// Chat 实现 loom.ChatModel.Chat,自动 retry。
func (m *Model) Chat(ctx context.Context, req loom.ChatRequest) (*loom.ChatResponse, error) {
	orReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	return loom.ChatWithRetry(ctx, classifier{}, m.retryCfg, func(callCtx context.Context) (*loom.ChatResponse, error) {
		return m.chatRaw(callCtx, orReq)
	})
}

// chatRaw 单次同步调用(无 retry)。
func (m *Model) chatRaw(ctx context.Context, orReq goOpenai.ChatCompletionRequest) (*loom.ChatResponse, error) {
	out, err := m.client.CreateChatCompletion(ctx, orReq)
	if err != nil {
		return nil, fmt.Errorf("loom/openrouter: chat: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("loom/openrouter: chat 返回 0 个 choice")
	}
	choice := out.Choices[0]
	return &loom.ChatResponse{
		Content:          choice.Message.Content,
		ReasoningContent: extractReasoning(choice.Message.ReasoningContent, choice.Message.ExtraFields),
		ToolCalls:        translateToolCalls(choice.Message.ToolCalls),
		FinishReason:     translateFinishReason(choice.FinishReason),
		Usage:            translateUsage(&out.Usage),
		Model:            out.Model,
	}, nil
}

// Stream 实现 loom.ChatModel.Stream,自动 retry(只 retry 到首帧探活前)。
// 强制开启 stream_options.include_usage,末尾帧拿 Usage。
func (m *Model) Stream(ctx context.Context, req loom.ChatRequest) (loom.Stream, error) {
	orReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	if orReq.StreamOptions == nil {
		orReq.StreamOptions = &goOpenai.StreamOptions{IncludeUsage: true}
	} else {
		orReq.StreamOptions.IncludeUsage = true
	}
	return loom.StreamWithRetry(ctx, classifier{}, m.retryCfg, func(streamCtx context.Context) (loom.Stream, error) {
		stream, err := m.client.CreateChatCompletionStream(streamCtx, orReq)
		if err != nil {
			return nil, fmt.Errorf("loom/openrouter: stream: %w", err)
		}
		return &streamAdapter{inner: stream}, nil
	})
}

// streamAdapter 把 go-openai ChatCompletionStream 包装成 loom.Stream。
type streamAdapter struct {
	inner *goOpenai.ChatCompletionStream
}

func (s *streamAdapter) Recv() (*loom.Chunk, error) {
	raw, err := s.inner.Recv()
	if err != nil {
		return nil, err // io.EOF 透传
	}

	chunk := &loom.Chunk{Model: raw.Model}
	if raw.Usage != nil {
		u := translateUsage(raw.Usage)
		chunk.Usage = &u
	}
	// include_usage 的末尾帧 choices=[];普通帧取首项 delta。
	if len(raw.Choices) > 0 {
		choice := raw.Choices[0]
		chunk.ContentDelta = choice.Delta.Content
		chunk.ReasoningContentDelta = extractReasoning(choice.Delta.ReasoningContent, choice.Delta.ExtraFields)
		chunk.ToolCallDeltas = translateToolCallDeltas(choice.Delta.ToolCalls)
		if choice.FinishReason != "" && choice.FinishReason != goOpenai.FinishReasonNull {
			chunk.FinishReason = translateFinishReason(choice.FinishReason)
		}
	}
	return chunk, nil
}

func (s *streamAdapter) Close() error {
	return s.inner.Close()
}

// buildRequest 把 loom.ChatRequest 翻译成 go-openai 请求结构。
func (m *Model) buildRequest(req loom.ChatRequest) (goOpenai.ChatCompletionRequest, error) {
	out := goOpenai.ChatCompletionRequest{
		Model:    m.name,
		Messages: translateMessages(req.Messages),
	}
	if req.Temperature != nil {
		out.Temperature = new(float32(*req.Temperature))
	}
	if req.TopP != nil {
		out.TopP = float32(*req.TopP)
	}
	if req.MaxTokens != nil {
		out.MaxTokens = *req.MaxTokens
	}
	out.Stop = req.Stop

	if err := loom.CheckRequestAgainstCapabilities(m.capabilities, req); err != nil {
		return goOpenai.ChatCompletionRequest{}, fmt.Errorf("loom/openrouter: %w", err)
	}
	resolved, err := loom.ResolveReasoning(m.capabilities, req.Reasoning)
	if err != nil {
		return goOpenai.ChatCompletionRequest{}, fmt.Errorf("loom/openrouter: %w", err)
	}
	reasoning, err := translateReasoning(resolved)
	if err != nil {
		return goOpenai.ChatCompletionRequest{}, err
	}
	if reasoning != nil {
		out.SetExtraFields(map[string]any{"reasoning": reasoning})
	}

	if req.StructuredOutput != nil {
		switch req.StructuredOutput.Mode {
		case loom.StructuredOutputJSONSchema:
			out.ResponseFormat = &goOpenai.ChatCompletionResponseFormat{
				Type: goOpenai.ChatCompletionResponseFormatTypeJSONSchema,
				JSONSchema: &goOpenai.ChatCompletionResponseFormatJSONSchema{
					Name:        req.StructuredOutput.Name,
					Description: req.StructuredOutput.Description,
					Schema:      req.StructuredOutput.Schema,
					// strict 固定 true:供应商硬保证输出合规永远是调用方想要的,见 loom.StructuredOutput 注释
					Strict: true,
				},
			}
		case loom.StructuredOutputJSONObject:
			out.ResponseFormat = &goOpenai.ChatCompletionResponseFormat{
				Type: goOpenai.ChatCompletionResponseFormatTypeJSONObject,
			}
		case loom.StructuredOutputUnsupported:
			// 不传
		case loom.StructuredOutputNone:
			// 请求侧不允许 none(能力声明专用),CheckRequestAgainstCapabilities 已前置拦截
			return goOpenai.ChatCompletionRequest{}, fmt.Errorf("loom/openrouter: StructuredOutput.Mode 不允许取 %q", req.StructuredOutput.Mode)
		default:
			return goOpenai.ChatCompletionRequest{}, fmt.Errorf("loom/openrouter: 未知 structured output mode %q", req.StructuredOutput.Mode)
		}
	} else {
		switch req.ResponseFormat {
		case loom.ResponseFormatJSONObject:
			out.ResponseFormat = &goOpenai.ChatCompletionResponseFormat{
				Type: goOpenai.ChatCompletionResponseFormatTypeJSONObject,
			}
		case loom.ResponseFormatText:
			out.ResponseFormat = &goOpenai.ChatCompletionResponseFormat{
				Type: goOpenai.ChatCompletionResponseFormatTypeText,
			}
		case loom.ResponseFormatDefault:
			// 不传
		default:
			// 未知格式不传(对齐 deepseek provider 行为)
		}
	}

	tools, err := translateTools(req.Tools)
	if err != nil {
		return goOpenai.ChatCompletionRequest{}, fmt.Errorf("loom/openrouter: 翻译 tools: %w", err)
	}
	out.Tools = tools
	if req.ToolChoice != nil {
		out.ToolChoice = translateToolChoice(req.ToolChoice)
	}
	return out, nil
}

// translateReasoning 把解析结果翻译成 OpenRouter 统一 reasoning 对象。
// 返回 nil 表示不发该字段(omit)。
func translateReasoning(resolved loom.ResolvedReasoning) (map[string]any, error) {
	switch resolved.Send {
	case loom.ReasoningSendOmit:
		return nil, nil
	case loom.ReasoningSendDisabled:
		return map[string]any{"enabled": false}, nil
	case loom.ReasoningSendEnabled:
		reasoning := map[string]any{"enabled": true}
		switch resolved.Effort {
		case loom.ReasoningEffortLow, loom.ReasoningEffortMedium, loom.ReasoningEffortHigh:
			reasoning["effort"] = string(resolved.Effort)
		case loom.ReasoningEffortMax:
			// OpenRouter 统一档位只有 low/medium/high(max 是 deepseek 专属),
			// 正常情况下 ResolveReasoning 已按 capabilities.ReasoningEfforts 提前拦截。
			return nil, fmt.Errorf("loom/openrouter: openrouter 不支持 reasoning effort %q(支持 low/medium/high)", resolved.Effort)
		case loom.ReasoningEffortDefault:
			// 只开推理,档位走模型默认
		default:
			return nil, fmt.Errorf("loom/openrouter: 未知 reasoning effort %q", resolved.Effort)
		}
		return reasoning, nil
	default:
		return nil, fmt.Errorf("loom/openrouter: 未知 reasoning send %q", resolved.Send)
	}
}

// extractReasoning 提取推理输出。OpenRouter 统一用 "reasoning" 字段(ExtraFields),
// 个别上游可能透传 deepseek 风格的 reasoning_content — 优先取后者(已结构化),
// 否则从 ExtraFields 解 "reasoning"。
func extractReasoning(reasoningContent string, extra map[string]json.RawMessage) string {
	if reasoningContent != "" {
		return reasoningContent
	}
	raw, ok := extra["reasoning"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// "reasoning" 不是 string(如 null 或对象),忽略
		return ""
	}
	return s
}

func translateMessages(msgs []loom.Message) []goOpenai.ChatCompletionMessage {
	out := make([]goOpenai.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		gm := goOpenai.ChatCompletionMessage{
			Role:    translateRole(m.Role),
			Content: m.Content,
			Name:    m.Name,
		}
		// ReasoningContent 不回传:OpenRouter 不要求(也不统一支持)把历史推理
		// 塞回上下文,assistant 历史只带最终答案与 tool_calls。
		if m.ToolCallID != "" {
			gm.ToolCallID = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			gm.ToolCalls = make([]goOpenai.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				gm.ToolCalls = append(gm.ToolCalls, goOpenai.ToolCall{
					ID:   tc.ID,
					Type: goOpenai.ToolTypeFunction,
					Function: goOpenai.FunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
		}
		out = append(out, gm)
	}
	return out
}

func translateRole(r loom.Role) string {
	switch r {
	case loom.RoleSystem:
		return goOpenai.ChatMessageRoleSystem
	case loom.RoleUser:
		return goOpenai.ChatMessageRoleUser
	case loom.RoleAssistant:
		return goOpenai.ChatMessageRoleAssistant
	case loom.RoleTool:
		return goOpenai.ChatMessageRoleTool
	default:
		// 未知角色按 user 兜底(与请求构造层的校验互补,实际不可达)
		return goOpenai.ChatMessageRoleUser
	}
}

func translateTools(tools []*loom.ToolInfo) ([]goOpenai.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]goOpenai.Tool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		var params json.RawMessage
		if t.Parameters != nil {
			b, err := json.Marshal(t.Parameters)
			if err != nil {
				return nil, fmt.Errorf("工具 %q 参数 schema marshal 失败: %w", t.Name, err)
			}
			params = b
		}
		out = append(out, goOpenai.Tool{
			Type: goOpenai.ToolTypeFunction,
			Function: &goOpenai.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out, nil
}

func translateToolChoice(tc *loom.ToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Mode {
	case loom.ToolChoiceAuto:
		return "auto"
	case loom.ToolChoiceNone:
		return "none"
	case loom.ToolChoiceRequired:
		return "required"
	case loom.ToolChoiceSpecific:
		return goOpenai.ToolChoice{
			Type:     goOpenai.ToolTypeFunction,
			Function: goOpenai.ToolFunction{Name: tc.Name},
		}
	default:
		// 未知 Mode 不传(走服务端默认,对齐 deepseek provider 行为)
		return nil
	}
}

func translateToolCalls(calls []goOpenai.ToolCall) []loom.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]loom.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, loom.ToolCall{
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: c.Function.Arguments,
		})
	}
	return out
}

func translateToolCallDeltas(deltas []goOpenai.ToolCall) []loom.ToolCallDelta {
	if len(deltas) == 0 {
		return nil
	}
	out := make([]loom.ToolCallDelta, 0, len(deltas))
	for _, d := range deltas {
		idx := 0
		if d.Index != nil {
			idx = *d.Index
		}
		out = append(out, loom.ToolCallDelta{
			Index:     idx,
			ID:        d.ID,
			Name:      d.Function.Name,
			Arguments: d.Function.Arguments,
		})
	}
	return out
}

func translateFinishReason(fr goOpenai.FinishReason) loom.FinishReason {
	switch fr {
	case goOpenai.FinishReasonStop:
		return loom.FinishReasonStop
	case goOpenai.FinishReasonLength:
		return loom.FinishReasonLength
	case goOpenai.FinishReasonToolCalls, goOpenai.FinishReasonFunctionCall:
		return loom.FinishReasonToolCalls
	case goOpenai.FinishReasonContentFilter:
		return loom.FinishReasonContentFilter
	case goOpenai.FinishReasonNull:
		return ""
	default:
		// OpenRouter 偶有自定义值(如上游 provider 透传),按自然停止处理
		return loom.FinishReasonStop
	}
}

func translateUsage(u *goOpenai.Usage) loom.Usage {
	if u == nil {
		return loom.Usage{}
	}
	out := loom.Usage{
		PromptTokens:     uint64(max(u.PromptTokens, 0)),
		CompletionTokens: uint64(max(u.CompletionTokens, 0)),
		TotalTokens:      uint64(max(u.TotalTokens, 0)),
	}
	if u.PromptTokensDetails != nil {
		out.CachedTokens = uint64(max(u.PromptTokensDetails.CachedTokens, 0))
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = uint64(max(u.CompletionTokensDetails.ReasoningTokens, 0))
	}
	return out
}
