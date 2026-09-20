// Package deepseek implements loom.ChatModel using the DeepSeek wire protocol
// (an OpenAI-compatible chat completions API) on top of the official
// github.com/openai/openai-go/v3 SDK.
//
// DeepSeek-specific differences from the plain OpenAI protocol, which is why
// this does not reuse the openrouter package:
//   - reasoning is switched with a request-level "thinking" object
//     ({"type": "enabled"|"disabled"}), injected through SetExtraFields;
//   - reasoning output arrives in the "reasoning_content" message field, read
//     from the SDK's ExtraFields;
//   - reasoning token presence is reported through
//     usage.completion_tokens_details.reasoning_tokens.
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
//	    Messages:  []loom.Message{{Role: loom.RoleUser, Content: "你好"}},
//	    Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: loom.ReasoningEffortHigh},
//	})
package deepseek

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"

	"github.com/loomagent/loom"
)

// DefaultBaseURL DeepSeek API 入口。
const DefaultBaseURL = "https://api.deepseek.com"

// 默认 model 别名。
const (
	ModelV4Flash = "deepseek-v4-flash"
	ModelV4Pro   = "deepseek-v4-pro"
)

// Config DeepSeek provider 构造参数。
type Config struct {
	// APIKey 必填。
	APIKey string
	// ModelName 必填,如 ModelV4Flash / ModelV4Pro;空时默认 ModelV4Flash。
	ModelName string
	// BaseURL 可空 — 不设时用 DefaultBaseURL。
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
	client       openai.Client
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

	baseURL := DefaultBaseURL
	if cfg.BaseURL != "" {
		baseURL = cfg.BaseURL
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
	return &Model{
		// loom owns every retry (ChatWithRetry / StreamWithRetry), so the SDK must
		// not retry underneath; its default retries would multiply the attempts and
		// bypass the shared rate-limit cooldown.
		client:       openai.NewClient(option.WithAPIKey(cfg.APIKey), option.WithBaseURL(baseURL), option.WithMaxRetries(0)),
		name:         name,
		retryCfg:     retryCfg,
		capabilities: capabilities,
	}, nil
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
func (m *Model) chatRaw(ctx context.Context, dsReq openai.ChatCompletionNewParams) (*loom.ChatResponse, error) {
	out, err := m.client.Chat.Completions.New(ctx, dsReq)
	if err != nil {
		return nil, fmt.Errorf("loom/deepseek: chat: %w", normalizeDeepSeekError(err))
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("loom/deepseek: chat 返回 0 个 choice")
	}
	choice := out.Choices[0]
	return &loom.ChatResponse{
		Content:          choice.Message.Content,
		ReasoningContent: extractReasoning(choice.Message.JSON.ExtraFields),
		ToolCalls:        translateToolCalls(choice.Message.ToolCalls),
		FinishReason:     translateFinishReason(choice.FinishReason),
		Usage:            translateUsage(&out.Usage),
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
	dsReq.StreamOptions.IncludeUsage = param.NewOpt(true)
	return loom.StreamWithRetry(ctx, classifier{}, m.retryCfg, func(streamCtx context.Context) (loom.Stream, error) {
		return &streamAdapter{inner: m.client.Chat.Completions.NewStreaming(streamCtx, dsReq)}, nil
	})
}

// streamAdapter maps the upstream SSE stream to loom.Stream.
type streamAdapter struct {
	inner *ssestream.Stream[openai.ChatCompletionChunk]
}

func (s *streamAdapter) Recv() (*loom.Chunk, error) {
	if !s.inner.Next() {
		if err := s.inner.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	raw := s.inner.Current()

	chunk := &loom.Chunk{Model: raw.Model}
	if raw.JSON.Usage.Valid() {
		u := translateUsage(&raw.Usage)
		chunk.Usage = &u
	}
	// DeepSeek 末尾会发一帧 choices=[] 的 Usage 帧;
	// 普通帧 choices 至少 1 项,取首项 delta。
	if len(raw.Choices) > 0 {
		choice := raw.Choices[0]
		chunk.ContentDelta = choice.Delta.Content
		chunk.ReasoningContentDelta = extractReasoning(choice.Delta.JSON.ExtraFields)
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

// buildRequest validates explicit configuration and serializes the requested
// protocol fields. It does not infer capabilities from the provider/model name.
func (m *Model) buildRequest(req loom.ChatRequest) (_ openai.ChatCompletionNewParams, err error) {
	defer func() { err = loom.LocalRequestError(err) }()
	out := openai.ChatCompletionNewParams{
		Model:    m.name,
		Messages: translateMessages(req.Messages),
	}
	if req.Temperature != nil {
		out.Temperature = param.NewOpt(*req.Temperature)
	}
	if req.TopP != nil {
		out.TopP = param.NewOpt(*req.TopP)
	}
	if req.MaxTokens != nil {
		out.MaxTokens = param.NewOpt(int64(*req.MaxTokens))
	}
	switch len(req.Stop) {
	case 0:
		// 不传
	case 1:
		out.Stop.OfString = param.NewOpt(req.Stop[0])
	default:
		out.Stop.OfStringArray = req.Stop
	}
	if err := loom.CheckRequestAgainstCapabilities(m.capabilities, req); err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: %w", err)
	}
	resolved, err := loom.ResolveModelReasoning("deepseek", m.name, m.capabilities, req.Reasoning)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: %w", err)
	}
	// thinking 是 DeepSeek 专有字段,SDK 没有类型化的位置,按需注入。
	// ReasoningSendOmit 时不注入,等价于不发该字段。
	switch resolved.Send {
	case loom.ReasoningSendEnabled:
		out.SetExtraFields(map[string]any{"thinking": map[string]any{"type": "enabled"}})
	case loom.ReasoningSendDisabled:
		out.SetExtraFields(map[string]any{"thinking": map[string]any{"type": "disabled"}})
	case loom.ReasoningSendOmit:
		// 不发 thinking 字段
	default:
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: 未知 reasoning send %q", resolved.Send)
	}
	out.ReasoningEffort = shared.ReasoningEffort(resolved.Effort)

	if req.StructuredOutput != nil {
		switch req.StructuredOutput.Mode {
		case loom.StructuredOutputJSONObject:
			out.ResponseFormat.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
		case loom.StructuredOutputJSONSchema:
			if req.StructuredOutput.Schema == nil {
				return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: json_schema structured output 缺少 schema")
			}
			out.ResponseFormat.OfJSONSchema = &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:        req.StructuredOutput.Name,
					Description: param.NewOpt(req.StructuredOutput.Description),
					Schema:      req.StructuredOutput.Schema,
					// strict 固定 true:供应商硬保证输出合规永远是调用方想要的,见 loom.StructuredOutput 注释
					Strict: param.NewOpt(true),
				},
			}
		case loom.StructuredOutputUnsupported:
			// 不传
		case loom.StructuredOutputNone:
			// 请求侧不允许 none(能力声明专用),CheckRequestAgainstCapabilities 已前置拦截
			return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: StructuredOutput.Mode 不允许取 %q", req.StructuredOutput.Mode)
		default:
			return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: 未知 structured output mode %q", req.StructuredOutput.Mode)
		}
	} else {
		switch req.ResponseFormat {
		case loom.ResponseFormatJSONObject:
			out.ResponseFormat.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
		case loom.ResponseFormatText:
			out.ResponseFormat.OfText = &shared.ResponseFormatTextParam{}
		case loom.ResponseFormatDefault:
			// 不传
		default:
			// 未知格式不传
		}
	}
	tools, err := translateTools(req.Tools)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: 翻译 tools: %w", err)
	}
	out.Tools = tools
	if req.ToolChoice != nil {
		out.ToolChoice = translateToolChoice(req.ToolChoice)
	}
	return out, nil
}

// normalizeDeepSeekError 把 DeepSeek 的业务错误映射成 loom 的哨兵错误。
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
	apiErr, ok := errors.AsType[*openai.Error](err)
	if !ok {
		return false
	}
	if apiErr.StatusCode != 400 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(apiErr.Message), "Content Exists Risk")
}

// extractReasoning 提取 DeepSeek 的推理输出。DeepSeek 用 message 级的
// "reasoning_content" 字段,SDK 没有类型化,放在 ExtraFields。
func extractReasoning(extra map[string]respjson.Field) string {
	field, ok := extra["reasoning_content"]
	if !ok {
		return ""
	}
	var s string
	if err := jsonv2.Unmarshal([]byte(field.Raw()), &s); err != nil {
		// 不是 string(如 null 或对象),忽略
		return ""
	}
	return s
}

func translateMessages(msgs []loom.Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		var gm openai.ChatCompletionMessageParamUnion
		switch m.Role {
		case loom.RoleSystem:
			gm = openai.SystemMessage(m.Content)
		case loom.RoleAssistant:
			gm = openai.AssistantMessage(m.Content)
			if m.Name != "" {
				gm.OfAssistant.Name = param.NewOpt(m.Name)
			}
			for _, tc := range m.ToolCalls {
				gm.OfAssistant.ToolCalls = append(gm.OfAssistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{ID: tc.ID, Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: tc.Name, Arguments: tc.Arguments}},
				})
			}
		case loom.RoleTool:
			gm = openai.ToolMessage(m.Content, m.ToolCallID)
		case loom.RoleUser:
			gm = openai.UserMessage(m.Content)
		default:
			gm = openai.UserMessage(m.Content)
		}
		out = append(out, gm)
	}
	return out
}

// translateTools 把 loom ToolInfo 翻译成 SDK 的 function tool。
// nil/空 输入返回 nil,SDK 视作"不带工具"。
func translateTools(tools []*loom.ToolInfo) ([]openai.ChatCompletionToolUnionParam, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		params := shared.FunctionParameters{"type": "object", "properties": map[string]any{}}
		if t.Parameters != nil {
			b, err := jsonv2.Marshal(t.Parameters)
			if err != nil {
				return nil, fmt.Errorf("工具 %q 参数 schema marshal 失败: %w", t.Name, err)
			}
			if err := jsonv2.Unmarshal(b, &params); err != nil {
				return nil, fmt.Errorf("工具 %q 参数 schema unmarshal 失败: %w", t.Name, err)
			}
		}
		out = append(out, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: param.NewOpt(t.Description),
			Parameters:  params,
		}))
	}
	return out, nil
}

func translateToolChoice(tc *loom.ToolChoice) openai.ChatCompletionToolChoiceOptionUnionParam {
	if tc == nil {
		return openai.ChatCompletionToolChoiceOptionUnionParam{}
	}
	switch tc.Mode {
	case loom.ToolChoiceAuto:
		return openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("auto")}
	case loom.ToolChoiceNone:
		return openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("none")}
	case loom.ToolChoiceRequired:
		return openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("required")}
	case loom.ToolChoiceSpecific:
		return openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{Name: tc.Name})
	default:
		// 未知 Mode 不传(走服务端默认)
		return openai.ChatCompletionToolChoiceOptionUnionParam{}
	}
}

// translateToolCalls 把同步响应的 ToolCalls 翻译成 loom 形式。
func translateToolCalls(calls []openai.ChatCompletionMessageToolCallUnion) []loom.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]loom.ToolCall, 0, len(calls))
	for _, c := range calls {
		if c.Type != "function" {
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

// translateToolCallDeltas 流式 ToolCall 增量翻译(字段重映射)。
func translateToolCallDeltas(deltas []openai.ChatCompletionChunkChoiceDeltaToolCall) []loom.ToolCallDelta {
	if len(deltas) == 0 {
		return nil
	}
	out := make([]loom.ToolCallDelta, 0, len(deltas))
	for _, d := range deltas {
		out = append(out, loom.ToolCallDelta{
			Index:     int(d.Index),
			ID:        d.ID,
			Name:      d.Function.Name,
			Arguments: d.Function.Arguments,
		})
	}
	return out
}

func translateFinishReason(r string) loom.FinishReason {
	switch r {
	case "stop":
		return loom.FinishReasonStop
	case "length":
		return loom.FinishReasonLength
	case "content_filter":
		return loom.FinishReasonContentFilter
	case "tool_calls", "function_call":
		return loom.FinishReasonToolCalls
	case "insufficient_system_resource":
		return loom.FinishReasonError
	case "":
		return ""
	default:
		return loom.FinishReason(r)
	}
}

func translateUsage(u *openai.CompletionUsage) loom.Usage {
	if u == nil {
		return loom.Usage{}
	}
	out := loom.Usage{
		PromptTokens:     uint64(max(u.PromptTokens, 0)),
		CompletionTokens: uint64(max(u.CompletionTokens, 0)),
		TotalTokens:      uint64(max(u.TotalTokens, 0)),
	}
	if u.JSON.PromptTokensDetails.Valid() {
		out.CachedTokens = uint64(max(u.PromptTokensDetails.CachedTokens, 0))
	}
	// DeepSeek reports the cache-hit count under its own key rather than
	// prompt_tokens_details.cached_tokens, so read the raw field as a fallback.
	if out.CachedTokens == 0 {
		if field, ok := u.JSON.ExtraFields["prompt_cache_hit_tokens"]; ok {
			var cached int64
			if err := jsonv2.Unmarshal([]byte(field.Raw()), &cached); err == nil && cached > 0 {
				out.CachedTokens = uint64(cached)
			}
		}
	}
	if u.JSON.CompletionTokensDetails.Valid() {
		out.ReasoningTokens = uint64(max(u.CompletionTokensDetails.ReasoningTokens, 0))
		out.ReasoningTokensKnown = u.CompletionTokensDetails.JSON.ReasoningTokens.Valid()
	}
	return out
}
