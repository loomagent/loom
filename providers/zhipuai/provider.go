// Package zhipuai implements the domestic Zhipu AI pay-as-you-go Chat API.
// Model capabilities are supplied by the caller; they are not inferred from model names.
package zhipuai

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/shared"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/internal/openaicompat"
)

// DefaultBaseURL is the Zhipu AI API endpoint.
const DefaultBaseURL = "https://open.bigmodel.cn/api/paas/v4"

// Config holds the Zhipu AI provider's construction parameters.
type Config struct {
	// APIKey is required.
	APIKey string
	// ModelName is required: the Zhipu AI model identifier, such as "glm-5.3".
	ModelName string
	// BaseURL may be empty, in which case DefaultBaseURL is used.
	BaseURL string

	// Retry controls the retry policy; nil means loom.DefaultRetryConfig().
	Retry *loom.RetryConfig
	// HTTPClient is optional, and replaces the client every request goes through. Use it
	// for a proxy, custom timeouts, or a test server's in-memory client.
	HTTPClient *http.Client

	// Capabilities is what the caller or modelfactory fills in from the model's real
	// configuration. nil leaves it undeclared, so capability checks pass requests through.
	Capabilities *loom.ModelCapabilities
}

// Model is one Zhipu AI model instance, implementing loom.ChatModel.
type Model struct {
	client       openai.Client
	name         string
	retryCfg     *loom.RetryConfig
	capabilities loom.ModelCapabilities
}

var _ loom.ChatModel = (*Model)(nil)

// New builds a Model.
func New(cfg Config) (*Model, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("loom/zhipuai: APIKey must not be empty")
	}
	if strings.TrimSpace(cfg.ModelName) == "" {
		return nil, fmt.Errorf("loom/zhipuai: ModelName must not be empty")
	}

	baseURL := DefaultBaseURL
	if cfg.BaseURL != "" {
		baseURL = cfg.BaseURL
	}

	retryCfg := cfg.Retry
	if retryCfg == nil {
		retryCfg = loom.DefaultRetryConfig()
	}
	// Capabilities come from the caller or modelfactory; no model default is hardcoded
	// here. Building the model without them leaves every capability undeclared, so each
	// check passes requests through.
	capabilities := loom.ModelCapabilities{}
	if cfg.Capabilities != nil {
		capabilities = *cfg.Capabilities
	}
	return &Model{
		client:       newClient(cfg, baseURL),
		name:         cfg.ModelName,
		retryCfg:     retryCfg,
		capabilities: capabilities,
	}, nil
}

// Name returns an identifier of the form "zhipuai/<model>".
func (m *Model) Name() string {
	return "zhipuai/" + m.name
}

// Capabilities returns the model capabilities declared at initialization.
func (m *Model) Capabilities() loom.ModelCapabilities {
	return m.capabilities
}

// Chat implements loom.ChatModel.Chat with automatic retries.
func (m *Model) Chat(ctx context.Context, req loom.ChatRequest) (*loom.ChatResponse, error) {
	orReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	return loom.ChatWithRetry(ctx, classifier{}, m.retryCfg, func(callCtx context.Context) (*loom.ChatResponse, error) {
		return m.chatRaw(callCtx, orReq)
	})
}

// chatRaw is one synchronous call with no retry.
func (m *Model) chatRaw(ctx context.Context, orReq openai.ChatCompletionNewParams) (*loom.ChatResponse, error) {
	out, err := m.client.Chat.Completions.New(ctx, orReq)
	if err != nil {
		return nil, fmt.Errorf("loom/zhipuai: chat: %w", normalizeError(err))
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("loom/zhipuai: chat returned 0 choices")
	}
	choice := out.Choices[0]
	if err := finishError(choice.FinishReason); err != nil {
		return nil, err
	}
	return &loom.ChatResponse{
		Content:          choice.Message.Content,
		ReasoningContent: extractReasoning(choice.Message.JSON.ExtraFields),
		ToolCalls:        openaicompat.ToolCalls(choice.Message.ToolCalls),
		FinishReason:     translateFinishReason(choice.FinishReason),
		Usage:            openaicompat.Usage(&out.Usage),
		Model:            out.Model,
	}, nil
}

// Stream implements loom.ChatModel.Stream with automatic retries, up to the first-frame
// liveness probe.
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

// streamAdapter wraps the OpenAI SDK's ChatCompletion stream as a loom.Stream.
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
		defer func() { _ = s.Close() }() // Preserve the stream terminal error; closing is cleanup.
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
		u := openaicompat.Usage(&raw.Usage)
		chunk.Usage = &u
	}
	// The trailing usage-only frame has choices=[]; an ordinary frame's first delta is
	// taken.
	if len(raw.Choices) > 0 {
		choice := raw.Choices[0]
		chunk.ContentDelta = choice.Delta.Content
		chunk.ReasoningContentDelta = extractReasoning(choice.Delta.JSON.ExtraFields)
		chunk.ToolCallDeltas = openaicompat.ToolCallDeltas(choice.Delta.ToolCalls)
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

// buildRequest translates a loom.ChatRequest into the go-openai request structure.
func (m *Model) buildRequest(req loom.ChatRequest) (_ openai.ChatCompletionNewParams, err error) {
	defer func() { err = loom.LocalRequestError(err) }()
	messages, err := translateMessages(req.Messages)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/zhipuai: translate messages: %w", err)
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
	resolved, err := loom.ResolveModelReasoning("zhipuai", m.name, m.capabilities, req.Reasoning)
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
			if req.StructuredOutput.Schema == nil {
				return out, fmt.Errorf("loom/zhipuai: json_schema structured output has no schema")
			}
			out.ResponseFormat.OfJSONSchema = &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:        loom.NormalizeStructuredOutputName(req.StructuredOutput.Name),
					Description: param.NewOpt(req.StructuredOutput.Description),
					Schema:      req.StructuredOutput.Schema,
					Strict:      param.NewOpt(true),
				},
			}
		case loom.StructuredOutputJSONObject:
			out.ResponseFormat.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
		case loom.StructuredOutputUnsupported:
			// send nothing
		case loom.StructuredOutputNone:
			// The request side forbids none, which is reserved for capability declarations;
			// CheckRequestAgainstCapabilities already rejects it earlier
			return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/zhipuai: StructuredOutput.Mode may not be %q", req.StructuredOutput.Mode)
		default:
			return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/zhipuai: unknown structured output mode %q", req.StructuredOutput.Mode)
		}
	} else {
		switch req.ResponseFormat {
		case loom.ResponseFormatJSONObject:
			out.ResponseFormat.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
		case loom.ResponseFormatText:
			out.ResponseFormat.OfText = &shared.ResponseFormatTextParam{}
		case loom.ResponseFormatDefault:
			// send nothing
		default:
			return out, fmt.Errorf("unknown response format %q", req.ResponseFormat)
		}
	}

	tools, err := openaicompat.Tools(req.Tools)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/zhipuai: translate tools: %w", err)
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
	if resolved.Effort != "" {
		extra["reasoning_effort"] = string(resolved.Effort)
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
			return nil, fmt.Errorf("message %d uses unknown role %q", len(out), m.Role)
		}
		out = append(out, gm)
	}
	return out, nil
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

// newClient builds the SDK client. Retries belong to loom, so the SDK must not retry
// underneath: its default retries would multiply the attempts and bypass the shared
// rate-limit cooldown.
func newClient(cfg Config, baseURL string) openai.Client {
	options := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithBaseURL(strings.TrimRight(baseURL, "/") + "/"),
		option.WithMaxRetries(0),
	}
	if cfg.HTTPClient != nil {
		options = append(options, option.WithHTTPClient(cfg.HTTPClient))
	}
	return openai.NewClient(options...)
}
