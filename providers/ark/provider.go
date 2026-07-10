// Package ark 实现 loom.ChatModel,底层走火山方舟 arkruntime SDK。
//
// 用法:
//
//	model, err := ark.New(ark.Config{
//	    APIKey:    os.Getenv("ARK_API_KEY"),
//	    ModelName: "ep-xxx",  // 火山方舟 endpoint id
//	})
//	if err != nil { ... }
//
//	resp, err := model.Chat(ctx, loom.ChatRequest{
//	    Messages: []loom.Message{{Role: loom.RoleUser, Content: "你好"}},
//	})
//
// Retry 内置(429 backoff + per-call timeout),通过 Config.Retry 调整或关闭。
package ark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	arkutils "github.com/volcengine/volcengine-go-sdk/service/arkruntime/utils"

	"github.com/loomagent/loom"
)

// Config ark provider 构造参数。
type Config struct {
	// APIKey 必填(走 BearerToken 鉴权)。
	APIKey string
	// ModelName 必填,火山方舟 endpoint id(如 "ep-20260301165020-2bltp")。
	ModelName string
	// BaseURL 可空 — 不设时用 arkruntime 默认 (https://ark.cn-beijing.volces.com/api/v3)。
	BaseURL string
	// Retry 控制 retry 策略;nil 走 loom.DefaultRetryConfig()。
	Retry *loom.RetryConfig
	// Capabilities 模型能力,由调用方或 modelfactory 按实际模型配置填充。
	// nil = 零值"未声明"(能力校验跳过、纯透传)。
	Capabilities *loom.ModelCapabilities
}

// Model 一个 ark 模型实例,实现 loom.ChatModel。
type Model struct {
	client       *arkruntime.Client
	name         string
	retryCfg     *loom.RetryConfig
	capabilities loom.ModelCapabilities
}

var _ loom.ChatModel = (*Model)(nil)

// New 构造 Model。
func New(cfg Config) (*Model, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("loom/ark: APIKey 不能为空")
	}
	if strings.TrimSpace(cfg.ModelName) == "" {
		return nil, fmt.Errorf("loom/ark: ModelName(endpoint id)不能为空")
	}
	var opts []arkruntime.ConfigOption
	if cfg.BaseURL != "" {
		opts = append(opts, arkruntime.WithBaseUrl(cfg.BaseURL))
	}
	client := arkruntime.NewClientWithApiKey(cfg.APIKey, opts...)
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
	return &Model{client: client, name: cfg.ModelName, retryCfg: retryCfg, capabilities: capabilities}, nil
}

// Name 返回 "ark/<endpoint>" 形式标识。
func (m *Model) Name() string {
	return "ark/" + m.name
}

// Capabilities 返回初始化时声明的模型能力。
func (m *Model) Capabilities() loom.ModelCapabilities {
	return m.capabilities
}

// Chat 实现 loom.ChatModel.Chat,自动 retry。
func (m *Model) Chat(ctx context.Context, req loom.ChatRequest) (*loom.ChatResponse, error) {
	arkReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	return loom.ChatWithRetry(ctx, classifier{}, m.retryCfg, func(callCtx context.Context) (*loom.ChatResponse, error) {
		return m.chatRaw(callCtx, arkReq)
	})
}

func (m *Model) chatRaw(ctx context.Context, req arkmodel.CreateChatCompletionRequest) (*loom.ChatResponse, error) {
	out, err := m.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("loom/ark: chat: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("loom/ark: chat 返回 0 个 choice")
	}
	choice := out.Choices[0]
	return &loom.ChatResponse{
		Content:          contentString(choice.Message.Content),
		ReasoningContent: derefString(choice.Message.ReasoningContent),
		ToolCalls:        translateToolCalls(choice.Message.ToolCalls),
		FinishReason:     translateFinishReason(choice.FinishReason),
		Usage:            translateUsage(&out.Usage),
		Model:            out.Model,
	}, nil
}

// Stream 实现 loom.ChatModel.Stream,自动 retry(只 retry 到首帧探活前)。
// 强制开启 stream_options.include_usage,这样末尾帧能拿到 Usage。
func (m *Model) Stream(ctx context.Context, req loom.ChatRequest) (loom.Stream, error) {
	arkReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	arkReq.Stream = new(true)
	if arkReq.StreamOptions == nil {
		arkReq.StreamOptions = &arkmodel.StreamOptions{IncludeUsage: true}
	} else {
		arkReq.StreamOptions.IncludeUsage = true
	}
	return loom.StreamWithRetry(ctx, classifier{}, m.retryCfg, func(streamCtx context.Context) (loom.Stream, error) {
		return m.streamRaw(streamCtx, arkReq)
	})
}

func (m *Model) streamRaw(ctx context.Context, req arkmodel.CreateChatCompletionRequest) (loom.Stream, error) {
	stream, err := m.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("loom/ark: stream: %w", err)
	}
	return &streamAdapter{inner: stream}, nil
}

// streamAdapter 把 arkruntime.ChatCompletionStreamReader 包装成 loom.Stream。
type streamAdapter struct {
	inner *arkutils.ChatCompletionStreamReader
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
	if len(raw.Choices) > 0 {
		choice := raw.Choices[0]
		chunk.ContentDelta = choice.Delta.Content
		chunk.ReasoningContentDelta = derefString(choice.Delta.ReasoningContent)
		chunk.ToolCallDeltas = translateToolCallDeltas(choice.Delta.ToolCalls)
		if choice.FinishReason != "" {
			chunk.FinishReason = translateFinishReason(choice.FinishReason)
		}
	}
	return chunk, nil
}

func (s *streamAdapter) Close() error {
	return s.inner.Close()
}

// buildRequest 把 loom.ChatRequest 翻译成 arkmodel.ChatCompletionRequest。
//
// Reasoning.Mode 必传并映射到请求级 Thinking 字段(enabled/disabled);
// thinking 绑死在 endpoint 配置、请求参数不生效的旧 endpoint,应在模型
// capabilities 中声明为 always_on/none,ResolveReasoning 会兜住(报错或 omit),
// 不会发出无效参数。
// Reasoning.Effort 映射到 reasoning_effort(doubao-seed 2.0 支持 low/medium/high;
// minimal=不思考不建模,用 Mode=Disabled 表达;max 是 deepseek 专属档位,ark 报错,
// 正常情况下 ResolveReasoning 已按 capabilities.ReasoningEfforts 提前拦截)。
func (m *Model) buildRequest(req loom.ChatRequest) (arkmodel.CreateChatCompletionRequest, error) {
	out := arkmodel.CreateChatCompletionRequest{
		Model:    m.name,
		Messages: translateMessages(req.Messages),
	}
	if err := loom.CheckRequestAgainstCapabilities(m.capabilities, req); err != nil {
		return arkmodel.CreateChatCompletionRequest{}, fmt.Errorf("loom/ark: %w", err)
	}
	resolved, err := loom.ResolveReasoning(m.capabilities, req.Reasoning)
	if err != nil {
		return arkmodel.CreateChatCompletionRequest{}, fmt.Errorf("loom/ark: %w", err)
	}
	switch resolved.Send {
	case loom.ReasoningSendEnabled:
		out.Thinking = &arkmodel.Thinking{Type: arkmodel.ThinkingTypeEnabled}
	case loom.ReasoningSendDisabled:
		out.Thinking = &arkmodel.Thinking{Type: arkmodel.ThinkingTypeDisabled}
	case loom.ReasoningSendOmit:
		// 不发 Thinking 字段
	default:
		return arkmodel.CreateChatCompletionRequest{}, fmt.Errorf("loom/ark: 未知 reasoning send %q", resolved.Send)
	}
	switch resolved.Effort {
	case loom.ReasoningEffortLow:
		out.ReasoningEffort = new(arkmodel.ReasoningEffortLow)
	case loom.ReasoningEffortMedium:
		out.ReasoningEffort = new(arkmodel.ReasoningEffortMedium)
	case loom.ReasoningEffortHigh:
		out.ReasoningEffort = new(arkmodel.ReasoningEffortHigh)
	case loom.ReasoningEffortMax:
		return arkmodel.CreateChatCompletionRequest{}, fmt.Errorf("loom/ark: ark 不支持 reasoning effort %q(支持 low/medium/high)", resolved.Effort)
	case loom.ReasoningEffortDefault:
		// 不传,走模型默认
	default:
		return arkmodel.CreateChatCompletionRequest{}, fmt.Errorf("loom/ark: 未知 reasoning effort %q", resolved.Effort)
	}
	if req.Temperature != nil {
		out.Temperature = new(float32(*req.Temperature))
	}
	if req.TopP != nil {
		out.TopP = new(float32(*req.TopP))
	}
	if req.MaxTokens != nil {
		out.MaxTokens = new(*req.MaxTokens)
	}
	if len(req.Stop) > 0 {
		out.Stop = req.Stop
	}
	if len(req.Tools) > 0 {
		out.Tools = translateTools(req.Tools)
	}
	if req.ToolChoice != nil {
		out.ToolChoice = translateToolChoice(req.ToolChoice)
	}
	if err := applyResponseFormat(&out, req); err != nil {
		return arkmodel.CreateChatCompletionRequest{}, err
	}
	return out, nil
}

func applyResponseFormat(out *arkmodel.CreateChatCompletionRequest, req loom.ChatRequest) error {
	if req.StructuredOutput != nil {
		switch req.StructuredOutput.Mode {
		case loom.StructuredOutputJSONSchema:
			if req.StructuredOutput.Schema == nil {
				return fmt.Errorf("loom/ark: json_schema structured output 缺少 schema")
			}
			schemaObj, err := loom.StructuredSchemaObject(req.StructuredOutput.Schema)
			if err != nil {
				return fmt.Errorf("loom/ark: structured output schema marshal: %w", err)
			}
			out.ResponseFormat = &arkmodel.ResponseFormat{
				Type: arkmodel.ResponseFormatJSONSchema,
				JSONSchema: &arkmodel.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:        loom.NormalizeStructuredOutputName(req.StructuredOutput.Name),
					Description: req.StructuredOutput.Description,
					Schema:      schemaObj,
					// strict 固定 true:供应商硬保证输出合规永远是调用方想要的,见 loom.StructuredOutput 注释
					Strict: true,
				},
			}
		case loom.StructuredOutputJSONObject:
			out.ResponseFormat = &arkmodel.ResponseFormat{Type: arkmodel.ResponseFormatJsonObject}
		case loom.StructuredOutputUnsupported:
			// 不传
		case loom.StructuredOutputNone:
			// 请求侧不允许 none(能力声明专用),CheckRequestAgainstCapabilities 已前置拦截
			return fmt.Errorf("loom/ark: StructuredOutput.Mode 不允许取 %q", req.StructuredOutput.Mode)
		default:
			return fmt.Errorf("loom/ark: 未知 structured output mode %q", req.StructuredOutput.Mode)
		}
		return nil
	}

	switch req.ResponseFormat {
	case loom.ResponseFormatJSONObject:
		out.ResponseFormat = &arkmodel.ResponseFormat{Type: arkmodel.ResponseFormatJsonObject}
	case loom.ResponseFormatText:
		out.ResponseFormat = &arkmodel.ResponseFormat{Type: arkmodel.ResponseFormatText}
	case loom.ResponseFormatDefault:
		// 不传
	default:
		// 未知格式不传
	}
	return nil
}

func translateMessages(msgs []loom.Message) []*arkmodel.ChatCompletionMessage {
	out := make([]*arkmodel.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		gm := &arkmodel.ChatCompletionMessage{Role: translateRole(m.Role)}
		// content:assistant 携带 tool_calls 时 content 可空
		if m.Role != loom.RoleAssistant || m.Content != "" || len(m.ToolCalls) <= 0 {
			content := m.Content
			gm.Content = &arkmodel.ChatCompletionMessageContent{StringValue: &content}
		}
		if m.ReasoningContent != "" {
			rc := m.ReasoningContent
			gm.ReasoningContent = &rc
		}
		if m.ToolCallID != "" {
			gm.ToolCallID = m.ToolCallID
		}
		if m.Name != "" {
			n := m.Name
			gm.Name = &n
		}
		if len(m.ToolCalls) > 0 {
			gm.ToolCalls = make([]*arkmodel.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				gm.ToolCalls = append(gm.ToolCalls, &arkmodel.ToolCall{
					ID:   tc.ID,
					Type: arkmodel.ToolTypeFunction,
					Function: arkmodel.FunctionCall{
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
		return arkmodel.ChatMessageRoleSystem
	case loom.RoleUser:
		return arkmodel.ChatMessageRoleUser
	case loom.RoleAssistant:
		return arkmodel.ChatMessageRoleAssistant
	case loom.RoleTool:
		return arkmodel.ChatMessageRoleTool
	default:
		return string(r)
	}
}

func translateTools(tools []*loom.ToolInfo) []*arkmodel.Tool {
	out := make([]*arkmodel.Tool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		var params json.RawMessage
		if t.Parameters != nil {
			b, err := json.Marshal(t.Parameters)
			if err == nil {
				params = b
			}
		}
		out = append(out, &arkmodel.Tool{
			Type: arkmodel.ToolTypeFunction,
			Function: &arkmodel.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
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
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		}
	default:
		return nil
	}
}

func translateToolCalls(calls []*arkmodel.ToolCall) []loom.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]loom.ToolCall, 0, len(calls))
	for _, c := range calls {
		if c == nil {
			continue
		}
		out = append(out, loom.ToolCall{
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: c.Function.Arguments,
		})
	}
	return out
}

func translateToolCallDeltas(calls []*arkmodel.ToolCall) []loom.ToolCallDelta {
	if len(calls) == 0 {
		return nil
	}
	out := make([]loom.ToolCallDelta, 0, len(calls))
	for _, c := range calls {
		if c == nil {
			continue
		}
		idx := 0
		if c.Index != nil {
			idx = *c.Index
		}
		out = append(out, loom.ToolCallDelta{
			Index:     idx,
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: c.Function.Arguments,
		})
	}
	return out
}

func translateFinishReason(r arkmodel.FinishReason) loom.FinishReason {
	switch r {
	case arkmodel.FinishReasonStop:
		return loom.FinishReasonStop
	case arkmodel.FinishReasonLength:
		return loom.FinishReasonLength
	case arkmodel.FinishReasonContentFilter:
		return loom.FinishReasonContentFilter
	case arkmodel.FinishReasonToolCalls:
		return loom.FinishReasonToolCalls
	case arkmodel.FinishReasonFunctionCall:
		return loom.FinishReasonToolCalls
	case arkmodel.FinishReasonNull:
		return ""
	case "":
		return ""
	default:
		return loom.FinishReason(r)
	}
}

func translateUsage(u *arkmodel.Usage) loom.Usage {
	if u == nil {
		return loom.Usage{}
	}
	return loom.Usage{
		PromptTokens:     uint64(u.PromptTokens),
		CompletionTokens: uint64(u.CompletionTokens),
		TotalTokens:      uint64(u.TotalTokens),
		CachedTokens:     uint64(u.PromptTokensDetails.CachedTokens),
		ReasoningTokens:  uint64(u.CompletionTokensDetails.ReasoningTokens),
	}
}

// contentString 把 ark 的 ChatCompletionMessageContent(string 或 part list)
// 翻译成 loom 用的纯字符串。Part list 形态把 text 部分拼起来,非 text part(image/audio/video)
// 在 loom 第一版纯文本场景下忽略。
func contentString(c *arkmodel.ChatCompletionMessageContent) string {
	if c == nil {
		return ""
	}
	if c.StringValue != nil {
		return *c.StringValue
	}
	var sb strings.Builder
	for _, p := range c.ListValue {
		if p != nil && p.Type == arkmodel.ChatCompletionMessageContentPartTypeText {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// 防止 io.EOF 被 unused import:provider 不直接用,但 streamAdapter.Recv 透传时
// 上层 StreamWithRetry 用 errors.Is 检查。
var _ = io.EOF

var _ = errors.As
