// Package zhipuai implements the domestic Zhipu AI pay-as-you-go Chat API.
// Model capabilities are supplied by the caller; they are not inferred from model names.
package zhipuai

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
	"github.com/openai/openai-go/v3/shared"

	"github.com/loomagent/loom"
)

// DefaultBaseURL Zhipu AI API 入口。
const DefaultBaseURL = "https://open.bigmodel.cn/api/paas/v4"

// Config Zhipu AI provider 构造参数。
type Config struct {
	// APIKey 必填。
	APIKey string
	// ModelName 必填,Zhipu AI 模型标识,如 "glm-5.3"。
	ModelName string
	// BaseURL 可空 — 不设时用 DefaultBaseURL。
	BaseURL string

	// Retry 控制 retry 策略;nil 走 loom.DefaultRetryConfig()。
	Retry *loom.RetryConfig

	// Capabilities 模型能力,由调用方或 modelfactory 按实际模型配置填充。
	// nil = 零值"未声明"(能力校验跳过、纯透传)。
	Capabilities *loom.ModelCapabilities
}

// Model 一个 Zhipu AI 模型实例,实现 loom.ChatModel。
type Model struct {
	client       openai.Client
	name         string
	retryCfg     *loom.RetryConfig
	capabilities loom.ModelCapabilities
}

var _ loom.ChatModel = (*Model)(nil)

// New 构造 Model。
func New(cfg Config) (*Model, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("loom/zhipuai: APIKey 不能为空")
	}
	if strings.TrimSpace(cfg.ModelName) == "" {
		return nil, fmt.Errorf("loom/zhipuai: ModelName 不能为空")
	}

	baseURL := DefaultBaseURL
	if cfg.BaseURL != "" {
		baseURL = cfg.BaseURL
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
		client:       openai.NewClient(option.WithAPIKey(cfg.APIKey), option.WithBaseURL(strings.TrimRight(baseURL, "/")+"/"), option.WithMaxRetries(0)),
		name:         cfg.ModelName,
		retryCfg:     retryCfg,
		capabilities: capabilities,
	}, nil
}

// Name 返回 "zhipuai/<model>" 形式标识。
func (m *Model) Name() string {
	return "zhipuai/" + m.name
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
func (m *Model) chatRaw(ctx context.Context, orReq openai.ChatCompletionNewParams) (*loom.ChatResponse, error) {
	out, err := m.client.Chat.Completions.New(ctx, orReq)
	if err != nil {
		return nil, fmt.Errorf("loom/zhipuai: chat: %w", normalizeError(err))
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("loom/zhipuai: chat 返回 0 个 choice")
	}
	choice := out.Choices[0]
	if err := finishError(choice.FinishReason); err != nil {
		return nil, err
	}
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
// Zhipu returns usage without OpenAI stream_options. Tool deltas require tool_stream.
func (m *Model) Stream(ctx context.Context, req loom.ChatRequest) (loom.Stream, error) {
	orReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	if len(orReq.Tools) > 0 {
		extra, err := translateReasoningFromRequest(m.capabilities, req.Reasoning)
		if err != nil {
			return nil, err
		}
		extra["tool_stream"] = true
		orReq.SetExtraFields(extra)
	}
	return loom.StreamWithRetry(ctx, classifier{}, m.retryCfg, func(streamCtx context.Context) (loom.Stream, error) {
		stream := m.client.Chat.Completions.NewStreaming(streamCtx, orReq)
		return &streamAdapter{inner: stream}, nil
	})
}

// streamAdapter 把 OpenAI SDK ChatCompletion stream 包装成 loom.Stream。
type streamAdapter struct {
	finished bool
	closed   bool
	inner    interface {
		Next() bool
		Current() openai.ChatCompletionChunk
		Err() error
		Close() error
	}
}

func (s *streamAdapter) Recv() (*loom.Chunk, error) {
	if s.closed {
		return nil, io.EOF
	}
	if !s.inner.Next() {
		defer s.Close()
		if err := s.inner.Err(); err != nil {
			return nil, normalizeError(err)
		}
		if !s.finished {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, io.EOF
	}
	raw := s.inner.Current()

	chunk := &loom.Chunk{Model: raw.Model}
	if raw.JSON.Usage.Valid() {
		u := translateUsage(&raw.Usage)
		chunk.Usage = &u
	}
	// usage-only 的末尾帧 choices=[];普通帧取首项 delta。
	if len(raw.Choices) > 0 {
		choice := raw.Choices[0]
		chunk.ContentDelta = choice.Delta.Content
		chunk.ReasoningContentDelta = extractReasoning(choice.Delta.JSON.ExtraFields)
		chunk.ToolCallDeltas = translateToolCallDeltas(choice.Delta.ToolCalls)
		if choice.FinishReason != "" {
			if err := finishError(choice.FinishReason); err != nil {
				_ = s.Close()
				return nil, err
			}
			s.finished = true
			chunk.FinishReason = translateFinishReason(choice.FinishReason)
		}
	}
	return chunk, nil
}

func (s *streamAdapter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.inner.Close()
}

// buildRequest 把 loom.ChatRequest 翻译成 go-openai 请求结构。
func (m *Model) buildRequest(req loom.ChatRequest) (openai.ChatCompletionNewParams, error) {
	messages, err := translateMessages(req.Messages)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/zhipuai: 翻译 messages: %w", err)
	}
	out := openai.ChatCompletionNewParams{
		Model:    m.name,
		Messages: messages,
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
	if len(req.Stop) > 4 {
		return out, fmt.Errorf("%w: zhipuai supports at most 4 stop strings", loom.ErrUnsupportedCapability)
	}
	if len(req.Stop) > 0 {
		out.Stop.OfStringArray = req.Stop
	}

	if err := loom.CheckRequestAgainstCapabilities(m.capabilities, req); err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/zhipuai: %w", err)
	}
	resolved, err := loom.ResolveReasoning(m.capabilities, req.Reasoning)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/zhipuai: %w", err)
	}
	reasoning, err := translateReasoning(resolved)
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	if reasoning != nil {
		out.SetExtraFields(reasoning)
	}

	if req.StructuredOutput != nil {
		switch req.StructuredOutput.Mode {
		case loom.StructuredOutputJSONSchema:
			return out, fmt.Errorf("%w: zhipuai supports json_object, not json_schema", loom.ErrUnsupportedCapability)
		case loom.StructuredOutputJSONObject:
			out.ResponseFormat.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
		case loom.StructuredOutputUnsupported:
			// 不传
		case loom.StructuredOutputNone:
			// 请求侧不允许 none(能力声明专用),CheckRequestAgainstCapabilities 已前置拦截
			return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/zhipuai: StructuredOutput.Mode 不允许取 %q", req.StructuredOutput.Mode)
		default:
			return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/zhipuai: 未知 structured output mode %q", req.StructuredOutput.Mode)
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
			return out, fmt.Errorf("unknown response format %q", req.ResponseFormat)
		}
	}

	tools, err := translateTools(req.Tools)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/zhipuai: 翻译 tools: %w", err)
	}
	out.Tools = tools
	if req.ToolChoice != nil {
		if req.ToolChoice.Mode != loom.ToolChoiceAuto {
			return out, fmt.Errorf("%w: zhipuai tool_choice only supports auto", loom.ErrUnsupportedCapability)
		}
		out.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("auto")}
	}
	return out, nil
}

// translateReasoning preserves explicit controls even for undeclared capabilities.
// This lets validation observe whether the service rejects or ignores a parameter.
func translateReasoning(resolved loom.ResolvedReasoning) (map[string]any, error) {
	extra := map[string]any{}
	switch resolved.Send {
	case loom.ReasoningSendOmit:
	case loom.ReasoningSendEnabled:
		extra["thinking"] = map[string]any{"type": "enabled"}
	case loom.ReasoningSendDisabled:
		extra["thinking"] = map[string]any{"type": "disabled"}
	default:
		return nil, fmt.Errorf("unknown reasoning send %q", resolved.Send)
	}
	switch resolved.Effort {
	case loom.ReasoningEffortDefault:
	case loom.ReasoningEffortLow, loom.ReasoningEffortMedium, loom.ReasoningEffortHigh, loom.ReasoningEffortMax:
		extra["reasoning_effort"] = string(resolved.Effort)
	default:
		return nil, fmt.Errorf("unknown reasoning effort %q", resolved.Effort)
	}
	return extra, nil
}

func translateReasoningFromRequest(caps loom.ModelCapabilities, r loom.Reasoning) (map[string]any, error) {
	resolved, err := loom.ResolveReasoning(caps, r)
	if err != nil {
		return nil, err
	}
	return translateReasoning(resolved)
}

func extractReasoning(extra map[string]respjson.Field) string {
	var value string
	if field, ok := extra["reasoning_content"]; ok {
		_ = jsonv2.Unmarshal([]byte(field.Raw()), &value)
	}
	return value
}

func translateMessages(msgs []loom.Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		var gm openai.ChatCompletionMessageParamUnion
		switch m.Role {
		case loom.RoleSystem:
			gm = openai.SystemMessage(m.Content)
		case loom.RoleAssistant:
			gm = openai.AssistantMessage(m.Content)
			if m.ReasoningContent != "" {
				gm.OfAssistant.SetExtraFields(map[string]any{"reasoning_content": m.ReasoningContent})
			}
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
			return nil, fmt.Errorf("消息 %d 使用未知角色 %q", len(out), m.Role)
		}
		out = append(out, gm)
	}
	return out, nil
}

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

func translateToolCallDeltas(deltas []openai.ChatCompletionChunkChoiceDeltaToolCall) []loom.ToolCallDelta {
	if len(deltas) == 0 {
		return nil
	}
	out := make([]loom.ToolCallDelta, 0, len(deltas))
	for _, d := range deltas {
		idx := int(d.Index)
		out = append(out, loom.ToolCallDelta{
			Index:     idx,
			ID:        d.ID,
			Name:      d.Function.Name,
			Arguments: d.Function.Arguments,
		})
	}
	return out
}

func translateFinishReason(fr string) loom.FinishReason {
	switch fr {
	case "stop":
		return loom.FinishReasonStop
	case "length":
		return loom.FinishReasonLength
	case "tool_calls", "function_call":
		return loom.FinishReasonToolCalls
	case "content_filter":
		return loom.FinishReasonContentFilter
	case "":
		return ""
	default:
		return loom.FinishReasonError
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
	if u.JSON.CompletionTokensDetails.Valid() {
		out.ReasoningTokens = uint64(max(u.CompletionTokensDetails.ReasoningTokens, 0))
		out.ReasoningTokensKnown = u.CompletionTokensDetails.JSON.ReasoningTokens.Valid()
	}
	return out
}

// finishError rejects provider failures even when HTTP itself succeeded.
func finishError(reason string) error {
	switch reason {
	case "stop", "length", "tool_calls":
		return nil
	case "sensitive", "content_filter":
		return loom.ErrSensitiveContentRisk
	case "network_error":
		return errors.New("loom/zhipuai: inference network_error")
	case "model_context_window_exceeded":
		return &APIError{Code: reason, Message: "model context window exceeded"}
	default:
		return &APIError{Code: "invalid_finish_reason", Message: fmt.Sprintf("unexpected finish_reason %q", reason)}
	}
}
