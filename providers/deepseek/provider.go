// Package deepseek 实现 loom.ChatModel,底层走 github.com/storynap/goseek。
//
// 用法:
//
//	model, err := deepseek.New(deepseek.Config{
//	    APIKey:    os.Getenv("DEEPSEEK_API_KEY"),
//	    ModelName: deepseek.ModelV4Flash,
//	})
//	if err != nil { ... }
//
//	resp, err := model.Chat(ctx, loom.ChatRequest{
//	    Messages: []loom.Message{
//	        {Role: loom.RoleUser, Content: "你好"},
//	    },
//	    Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: loom.ReasoningEffortHigh},
//	})
package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	goseek "github.com/storynap/goseek"

	"github.com/loomagent/loom"
)

// 默认 model 别名,跟 goseek 常量保持一致以便调用方按需引用。
const (
	ModelV4Flash = goseek.ModelDeepSeekV4Flash
	ModelV4Pro   = goseek.ModelDeepSeekV4Pro
)

// Config DeepSeek provider 构造参数。
type Config struct {
	// APIKey 必填。
	APIKey string
	// ModelName 必填,如 ModelV4Flash / ModelV4Pro;空时默认 ModelV4Flash。
	ModelName string
	// BaseURL 可空 — 不设时用 goseek 默认 (https://api.deepseek.com)。
	BaseURL string

	// Retry 控制 retry 策略;nil 走 loom.DefaultRetryConfig()(默认开启)。
	// 想完全关掉 retry,传 &loom.RetryConfig{MaxRetries: -1}(MaxRetries<0 时
	// 任意 Transient 都走一次后立即放弃 — 但 RateLimit 还是无限 retry,
	// 这是 provider 限流的兜底语义,业务不应该关)。
	Retry *loom.RetryConfig

	// Capabilities 模型能力,由调用方或 modelfactory 按实际模型配置填充。
	// nil = 零值"未声明"(能力校验跳过、纯透传)。
	Capabilities *loom.ModelCapabilities
}

// Model 一个 DeepSeek 模型实例,实现 loom.ChatModel。
type Model struct {
	client       *goseek.Client
	name         string
	retryCfg     *loom.RetryConfig
	capabilities loom.ModelCapabilities
}

// 编译期保证接口实现。
var _ loom.ChatModel = (*Model)(nil)

// New 构造 Model。
func New(cfg Config) (*Model, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("loom/deepseek: APIKey 不能为空")
	}

	var opts []goseek.Option
	if cfg.BaseURL != "" {
		opts = append(opts, goseek.WithBaseURL(cfg.BaseURL))
	}
	client, err := goseek.NewClient(cfg.APIKey, opts...)
	if err != nil {
		return nil, fmt.Errorf("loom/deepseek: 创建 client: %w", err)
	}

	name := cfg.ModelName
	if name == "" {
		name = ModelV4Flash
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
	return &Model{client: client, name: name, retryCfg: retryCfg, capabilities: capabilities}, nil
}

// Name 返回 "deepseek/<model>" 形式标识。
func (m *Model) Name() string {
	return "deepseek/" + m.name
}

// Capabilities 返回初始化时声明的模型能力。
func (m *Model) Capabilities() loom.ModelCapabilities {
	return m.capabilities
}

// Chat 实现 loom.ChatModel.Chat,自动 retry。
// retry 策略由 m.retryCfg 控制,见 Config.Retry。错误分类由本 provider 的
// classifier{} 提供(状态码 → ErrorClass)。
func (m *Model) Chat(ctx context.Context, req loom.ChatRequest) (*loom.ChatResponse, error) {
	dsReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	return loom.ChatWithRetry(ctx, classifier{}, m.retryCfg, func(callCtx context.Context) (*loom.ChatResponse, error) {
		return m.chatRaw(callCtx, dsReq)
	})
}

// chatRaw 单次同步 Chat 调用(无 retry,供 retry helper 反复调)。
func (m *Model) chatRaw(ctx context.Context, dsReq goseek.ChatCompletionRequest) (*loom.ChatResponse, error) {
	out, err := m.client.CreateChatCompletion(ctx, dsReq)
	if err != nil {
		return nil, fmt.Errorf("loom/deepseek: chat: %w", normalizeDeepSeekError(err))
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("loom/deepseek: chat 返回 0 个 choice")
	}
	choice := out.Choices[0]
	return &loom.ChatResponse{
		Content:          derefString(choice.Message.Content),
		ReasoningContent: derefString(choice.Message.ReasoningContent),
		ToolCalls:        translateToolCalls(choice.Message.ToolCalls),
		FinishReason:     translateFinishReason(choice.FinishReason),
		Usage:            translateUsage(out.Usage),
		Model:            out.Model,
	}, nil
}

// Stream 实现 loom.ChatModel.Stream,自动 retry(只 retry 到首帧探活前)。
// 一旦 stream 开始消费(业务方拿到第二帧及之后)就不再 retry。
// 默认强制开启 stream_options.include_usage,这样末尾帧能拿到 Usage。
func (m *Model) Stream(ctx context.Context, req loom.ChatRequest) (loom.Stream, error) {
	dsReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	if dsReq.StreamOptions == nil {
		dsReq.StreamOptions = &goseek.StreamOptions{IncludeUsage: true}
	} else {
		dsReq.StreamOptions.IncludeUsage = true
	}
	return loom.StreamWithRetry(ctx, classifier{}, m.retryCfg, func(streamCtx context.Context) (loom.Stream, error) {
		return m.streamRaw(streamCtx, dsReq)
	})
}

// streamRaw 单次 Stream 调用(无 retry)。
func (m *Model) streamRaw(ctx context.Context, dsReq goseek.ChatCompletionRequest) (loom.Stream, error) {
	stream, err := m.client.CreateChatCompletionStream(ctx, dsReq)
	if err != nil {
		return nil, fmt.Errorf("loom/deepseek: stream: %w", normalizeDeepSeekError(err))
	}
	return &streamAdapter{inner: stream}, nil
}

func normalizeDeepSeekError(err error) error {
	if err == nil {
		return nil
	}
	if isDeepSeekContentExistsRisk(err) {
		return fmt.Errorf("%w: %w", loom.ErrSensitiveContentRisk, err)
	}
	return err
}

func isDeepSeekContentExistsRisk(err error) bool {
	var apiErr *goseek.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != 400 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(apiErr.Message), "Content Exists Risk")
}

// streamAdapter 把 goseek.ChatCompletionStream 包装成 loom.Stream。
type streamAdapter struct {
	inner *goseek.ChatCompletionStream
}

func (s *streamAdapter) Recv() (*loom.Chunk, error) {
	raw, err := s.inner.Recv()
	if err != nil {
		return nil, err // io.EOF 透传
	}
	if raw == nil {
		return nil, nil
	}

	chunk := &loom.Chunk{Model: raw.Model}
	if raw.Usage != nil {
		u := translateUsage(raw.Usage)
		chunk.Usage = &u
	}
	// DeepSeek 末尾会发一帧 choices=[] 的 Usage 帧;
	// 普通帧 choices 至少 1 项,取首项 delta。
	if len(raw.Choices) > 0 {
		choice := raw.Choices[0]
		chunk.ContentDelta = derefString(choice.Delta.Content)
		chunk.ReasoningContentDelta = derefString(choice.Delta.ReasoningContent)
		chunk.ToolCallDeltas = translateToolCallDeltas(choice.Delta.ToolCalls)
		if choice.FinishReason != nil {
			chunk.FinishReason = translateFinishReason(*choice.FinishReason)
		}
	}
	return chunk, nil
}

func (s *streamAdapter) Close() error {
	return s.inner.Close()
}

// buildRequest 把 loom.ChatRequest 翻译成 goseek 请求结构。
// 仅 schema marshal 失败时返错(json.Marshal *jsonschema.Schema 走自定义 MarshalJSON)。
func (m *Model) buildRequest(req loom.ChatRequest) (goseek.ChatCompletionRequest, error) {
	out := goseek.ChatCompletionRequest{
		Model:       m.name,
		Messages:    translateMessages(req.Messages),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
	}
	switch len(req.Stop) {
	case 0:
		// 不传
	case 1:
		out.Stop = goseek.Stop(req.Stop[0])
	default:
		out.Stop = goseek.Stops(req.Stop...)
	}
	if err := loom.CheckRequestAgainstCapabilities(m.capabilities, req); err != nil {
		return goseek.ChatCompletionRequest{}, fmt.Errorf("loom/deepseek: %w", err)
	}
	resolved, err := loom.ResolveReasoning(m.capabilities, req.Reasoning)
	if err != nil {
		return goseek.ChatCompletionRequest{}, fmt.Errorf("loom/deepseek: %w", err)
	}
	switch resolved.Send {
	case loom.ReasoningSendEnabled:
		out.Thinking = goseek.EnableThinking()
	case loom.ReasoningSendDisabled:
		out.Thinking = goseek.DisableThinking()
	case loom.ReasoningSendOmit:
		// 不发 thinking 字段
	default:
		return goseek.ChatCompletionRequest{}, fmt.Errorf("loom/deepseek: 未知 reasoning send %q", resolved.Send)
	}
	switch resolved.Effort {
	case loom.ReasoningEffortHigh:
		out.ReasoningEffort = goseek.ReasoningEffortHigh
	case loom.ReasoningEffortMax:
		out.ReasoningEffort = goseek.ReasoningEffortMax
	case loom.ReasoningEffortLow, loom.ReasoningEffortMedium:
		// deepseek API 只有 high/max 两档(doubao 专属档位),正常情况下
		// ResolveReasoning 已按 capabilities.ReasoningEfforts 提前拦截。
		return goseek.ChatCompletionRequest{}, fmt.Errorf("loom/deepseek: deepseek 不支持 reasoning effort %q(支持 high/max)", resolved.Effort)
	case loom.ReasoningEffortDefault:
		// 不传,goseek 用默认
	default:
		return goseek.ChatCompletionRequest{}, fmt.Errorf("loom/deepseek: 未知 reasoning effort %q", resolved.Effort)
	}
	if req.StructuredOutput != nil {
		switch req.StructuredOutput.Mode {
		case loom.StructuredOutputJSONObject:
			out.ResponseFormat = goseek.JSONResponseFormat()
		case loom.StructuredOutputJSONSchema:
			return goseek.ChatCompletionRequest{}, fmt.Errorf("loom/deepseek: 不支持 json_schema structured output")
		case loom.StructuredOutputUnsupported:
			// 不传
		case loom.StructuredOutputNone:
			// 请求侧不允许 none(能力声明专用),CheckRequestAgainstCapabilities 已前置拦截
			return goseek.ChatCompletionRequest{}, fmt.Errorf("loom/deepseek: StructuredOutput.Mode 不允许取 %q", req.StructuredOutput.Mode)
		default:
			return goseek.ChatCompletionRequest{}, fmt.Errorf("loom/deepseek: 未知 structured output mode %q", req.StructuredOutput.Mode)
		}
	} else {
		switch req.ResponseFormat {
		case loom.ResponseFormatJSONObject:
			out.ResponseFormat = goseek.JSONResponseFormat()
		case loom.ResponseFormatText:
			out.ResponseFormat = goseek.TextResponseFormat()
		case loom.ResponseFormatDefault:
			// 不传
		default:
			// 未知格式不传
		}
	}
	tools, err := translateTools(req.Tools)
	if err != nil {
		return goseek.ChatCompletionRequest{}, fmt.Errorf("loom/deepseek: 翻译 tools: %w", err)
	}
	out.Tools = tools
	if req.ToolChoice != nil {
		out.ToolChoice = translateToolChoice(req.ToolChoice)
	}
	return out, nil
}

func translateMessages(msgs []loom.Message) []goseek.Message {
	out := make([]goseek.Message, 0, len(msgs))
	for _, m := range msgs {
		gm := goseek.Message{
			Role: translateRole(m.Role),
			Name: m.Name,
		}
		// assistant 携带 tool_calls 时,Content 可能合法为空。
		// 其它角色 Content 始终透传(空字符串由 goseek validate 报错给调用方)。
		if m.Role == loom.RoleAssistant && m.Content == "" && len(m.ToolCalls) > 0 {
			gm.Content = nil
		} else {
			gm.Content = new(m.Content)
		}
		if m.ReasoningContent != "" {
			gm.ReasoningContent = new(m.ReasoningContent)
		}
		if m.ToolCallID != "" {
			gm.ToolCallID = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			gm.ToolCalls = make([]goseek.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				gm.ToolCalls = append(gm.ToolCalls, goseek.ToolCall{
					ID:   tc.ID,
					Type: goseek.ToolTypeFunction,
					Function: goseek.FunctionCall{
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

// translateTools 把 loom ToolInfo 翻译成 goseek Tool。
// nil/空 输入返回 nil,goseek 视作"不带工具"。
func translateTools(tools []*loom.ToolInfo) ([]goseek.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]goseek.Tool, 0, len(tools))
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
		out = append(out, goseek.Tool{
			Type: goseek.ToolTypeFunction,
			Function: goseek.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out, nil
}

func translateToolChoice(tc *loom.ToolChoice) *goseek.ToolChoice {
	if tc == nil {
		return nil
	}
	switch tc.Mode {
	case loom.ToolChoiceAuto:
		return goseek.AutoToolChoice()
	case loom.ToolChoiceNone:
		return goseek.NoToolChoice()
	case loom.ToolChoiceRequired:
		return goseek.RequiredToolChoice()
	case loom.ToolChoiceSpecific:
		return goseek.NamedToolChoice(tc.Name)
	default:
		// 未知 Mode 不传(走 goseek 默认)
		return nil
	}
}

// translateToolCalls 把 goseek 同步响应的 ToolCalls 翻译成 loom 形式。
func translateToolCalls(calls []goseek.ToolCall) []loom.ToolCall {
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

// translateToolCallDeltas 流式 ToolCall 增量翻译。
// goseek DeltaMessage.ToolCalls 已经是 per-call 的增量片段,只需字段重映射。
func translateToolCallDeltas(deltas []goseek.ToolCallDelta) []loom.ToolCallDelta {
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

func translateRole(r loom.Role) goseek.MessageRole {
	switch r {
	case loom.RoleSystem:
		return goseek.MessageRoleSystem
	case loom.RoleUser:
		return goseek.MessageRoleUser
	case loom.RoleAssistant:
		return goseek.MessageRoleAssistant
	case loom.RoleTool:
		return goseek.MessageRoleTool
	default:
		return goseek.MessageRole(r)
	}
}

func translateFinishReason(r goseek.FinishReason) loom.FinishReason {
	switch r {
	case goseek.FinishReasonStop:
		return loom.FinishReasonStop
	case goseek.FinishReasonLength:
		return loom.FinishReasonLength
	case goseek.FinishReasonContentFilter:
		return loom.FinishReasonContentFilter
	case goseek.FinishReasonToolCalls:
		return loom.FinishReasonToolCalls
	case goseek.FinishReasonInsufficientSystemResource:
		return loom.FinishReasonError
	case "":
		return ""
	default:
		return loom.FinishReason(r)
	}
}

func translateUsage(u *goseek.Usage) loom.Usage {
	if u == nil {
		return loom.Usage{}
	}
	out := loom.Usage{
		PromptTokens:     uint64(u.PromptTokens),
		CompletionTokens: uint64(u.CompletionTokens),
		CachedTokens:     uint64(u.PromptCacheHitTokens),
		TotalTokens:      uint64(u.TotalTokens),
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = uint64(u.CompletionTokensDetails.ReasoningTokens)
	}
	return out
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
