// Package ark implements loom.ChatModel on top of the Volcengine Ark arkruntime SDK.
//
// Usage:
//
//	model, err := ark.New(ark.Config{
//	    APIKey:    os.Getenv("ARK_API_KEY"),
//	    ModelName: "ep-xxx",  // the Ark endpoint ID
//	})
//	if err != nil { ... }
//
//	resp, err := model.Chat(ctx, loom.ChatRequest{
//	    Messages: []loom.Message{{Role: loom.RoleUser, Content: "hello"}},
//	})
//
// Retries are built in: 429 backoff and a per-call timeout, adjusted or disabled through
// Config.Retry.
package ark

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	arkutils "github.com/volcengine/volcengine-go-sdk/service/arkruntime/utils"

	"github.com/loomagent/loom"
)

// DefaultBaseURL is the public Ark chat-completions endpoint used when Config
// does not provide an override.
const DefaultBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

// Config holds the Ark provider's construction parameters.
type Config struct {
	// APIKey is required, and is sent as a bearer token.
	APIKey string
	// ModelName is required: the Ark endpoint ID, such as "ep-20260301165020-2bltp".
	ModelName string
	// BaseURL may be empty, in which case the arkruntime default
	// (https://ark.cn-beijing.volces.com/api/v3) is used.
	BaseURL string
	// Retry controls the retry policy; nil means loom.DefaultRetryConfig().
	Retry *loom.RetryConfig
	// Capabilities is what the caller or modelfactory fills in from the model's real
	// configuration. nil leaves it undeclared, so capability checks pass requests through.
	Capabilities *loom.ModelCapabilities
	// RequestHeaders adds provider-specific headers to every chat and stream
	// request. The map is snapshotted by New. Callers should use this only for
	// endpoint policy negotiated with their Ark account, never for per-user data.
	RequestHeaders map[string]string
}

// Model is one Ark model instance, implementing loom.ChatModel.
type Model struct {
	client       *arkruntime.Client
	name         string
	retryCfg     *loom.RetryConfig
	capabilities loom.ModelCapabilities
	requestOpts  []arkruntime.RequestOption
}

var _ loom.ChatModel = (*Model)(nil)

// New builds a Model.
func New(cfg Config) (*Model, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("loom/ark: APIKey must not be empty")
	}
	if strings.TrimSpace(cfg.ModelName) == "" {
		return nil, fmt.Errorf("loom/ark: ModelName (the endpoint ID) must not be empty")
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	opts := []arkruntime.ConfigOption{arkruntime.WithBaseUrl(baseURL)}
	opts = withUsageTransport(cfg.APIKey, opts)
	client := arkruntime.NewClientWithApiKey(cfg.APIKey, opts...)
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
	requestOpts := make([]arkruntime.RequestOption, 0, len(cfg.RequestHeaders))
	for key, value := range cfg.RequestHeaders {
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("loom/ark: RequestHeaders contains an empty key")
		}
		requestOpts = append(requestOpts, arkruntime.WithCustomHeader(key, value))
	}
	return &Model{
		client: client, name: cfg.ModelName, retryCfg: retryCfg,
		capabilities: capabilities, requestOpts: requestOpts,
	}, nil
}

// Name returns an identifier of the form "ark/<endpoint>".
func (m *Model) Name() string {
	return "ark/" + m.name
}

// Capabilities returns the model capabilities declared at initialization.
func (m *Model) Capabilities() loom.ModelCapabilities {
	return m.capabilities
}

// Chat implements loom.ChatModel.Chat with automatic retries.
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
	ctx, evidence := captureUsage(ctx)
	out, err := m.client.CreateChatCompletion(ctx, req, m.requestOpts...)
	if err != nil {
		return nil, fmt.Errorf("loom/ark: chat: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("loom/ark: chat returned 0 choices")
	}
	choice := out.Choices[0]
	return &loom.ChatResponse{
		Content:          contentString(choice.Message.Content),
		ReasoningContent: derefString(choice.Message.ReasoningContent),
		ToolCalls:        translateToolCalls(choice.Message.ToolCalls),
		FinishReason:     translateFinishReason(choice.FinishReason),
		Usage:            translateUsage(&out.Usage, evidence.known()),
		Model:            out.Model,
	}, nil
}

// Stream implements loom.ChatModel.Stream with automatic retries, up to the first-frame
// liveness probe. stream_options.include_usage is forced on, so the last frame carries
// Usage.
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
	stream, err := m.client.CreateChatCompletionStream(ctx, req, m.requestOpts...)
	if err != nil {
		return nil, fmt.Errorf("loom/ark: stream: %w", err)
	}
	decoder := &usageUnmarshaler{inner: stream.Unmarshaler}
	stream.Unmarshaler = decoder
	return &streamAdapter{inner: stream, usageDecoder: decoder}, nil
}

// streamAdapter wraps an arkruntime.ChatCompletionStreamReader as a loom.Stream.
type streamAdapter struct {
	inner        *arkutils.ChatCompletionStreamReader
	usageDecoder *usageUnmarshaler
}

func (s *streamAdapter) Recv() (*loom.Chunk, error) {
	raw, err := s.inner.Recv()
	if err != nil {
		return nil, err // io.EOF passes through
	}
	chunk := &loom.Chunk{Model: raw.Model}
	if raw.Usage != nil {
		u := translateUsage(raw.Usage, s.usageDecoder != nil && s.usageDecoder.evidence.known())
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

// buildRequest translates a loom.ChatRequest into an arkmodel.ChatCompletionRequest.
//
// Reasoning.Mode is required and maps onto the request-level Thinking field, enabled or
// disabled. An older endpoint where thinking is fixed in the endpoint's own configuration,
// so request parameters have no effect, should declare always_on or none in the model's
// capabilities: ResolveReasoning then either fails the call or omits the parameter, so no
// ineffective parameter is ever sent.
// Reasoning.Effort is sent unchanged after provider/model contract validation.
func (m *Model) buildRequest(req loom.ChatRequest) (_ arkmodel.CreateChatCompletionRequest, err error) {
	defer func() { err = loom.LocalRequestError(err) }()
	out := arkmodel.CreateChatCompletionRequest{
		Model:    m.name,
		Messages: translateMessages(req.Messages),
	}
	if err := loom.CheckRequestAgainstCapabilities(m.capabilities, req); err != nil {
		return arkmodel.CreateChatCompletionRequest{}, fmt.Errorf("loom/ark: %w", err)
	}
	resolved, err := loom.ResolveModelReasoning("ark", m.name, m.capabilities, req.Reasoning)
	if err != nil {
		return arkmodel.CreateChatCompletionRequest{}, fmt.Errorf("loom/ark: %w", err)
	}
	switch resolved.Send {
	case loom.ReasoningSendEnabled:
		out.Thinking = &arkmodel.Thinking{Type: arkmodel.ThinkingTypeEnabled}
	case loom.ReasoningSendDisabled:
		out.Thinking = &arkmodel.Thinking{Type: arkmodel.ThinkingTypeDisabled}
	case loom.ReasoningSendOmit:
		// send no Thinking field
	default:
		return arkmodel.CreateChatCompletionRequest{}, fmt.Errorf("loom/ark: unknown reasoning send %q", resolved.Send)
	}
	// Raw effort is administrator-declared; service_tier is a separate parameter.
	// Reference: https://console.volcengine.com/ark/region:cn-beijing/docs/82379/2662855?lang=zh
	if resolved.Effort != loom.ReasoningEffortDefault {
		out.ReasoningEffort = new(arkmodel.ReasoningEffort(resolved.Effort))
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
				return fmt.Errorf("loom/ark: json_schema structured output has no schema")
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
					// strict is always true: a provider's hard guarantee that the output conforms
					// is always what the caller wants; see the loom.StructuredOutput comment
					Strict: true,
				},
			}
		case loom.StructuredOutputJSONObject:
			out.ResponseFormat = &arkmodel.ResponseFormat{Type: arkmodel.ResponseFormatJsonObject}
		case loom.StructuredOutputUnsupported:
			// send nothing
		case loom.StructuredOutputNone:
			// The request side forbids none, which is reserved for capability declarations;
			// CheckRequestAgainstCapabilities already rejects it earlier
			return fmt.Errorf("loom/ark: StructuredOutput.Mode may not be %q", req.StructuredOutput.Mode)
		default:
			return fmt.Errorf("loom/ark: unknown structured output mode %q", req.StructuredOutput.Mode)
		}
		return nil
	}

	switch req.ResponseFormat {
	case loom.ResponseFormatJSONObject:
		out.ResponseFormat = &arkmodel.ResponseFormat{Type: arkmodel.ResponseFormatJsonObject}
	case loom.ResponseFormatText:
		out.ResponseFormat = &arkmodel.ResponseFormat{Type: arkmodel.ResponseFormatText}
	case loom.ResponseFormatDefault:
		// send nothing
	default:
		// An unknown format sends nothing
	}
	return nil
}

func translateMessages(msgs []loom.Message) []*arkmodel.ChatCompletionMessage {
	out := make([]*arkmodel.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		gm := &arkmodel.ChatCompletionMessage{Role: translateRole(m.Role)}
		// content may be empty when an assistant message carries tool calls
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
		var params []byte
		if t.Parameters != nil {
			b, err := jsonv2.Marshal(t.Parameters)
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

func translateUsage(u *arkmodel.Usage, reasoningKnown bool) loom.Usage {
	if u == nil {
		return loom.Usage{}
	}
	return loom.Usage{
		PromptTokens:         uint64(u.PromptTokens),
		CompletionTokens:     uint64(u.CompletionTokens),
		TotalTokens:          uint64(u.TotalTokens),
		CachedTokens:         uint64(u.PromptTokensDetails.CachedTokens),
		ReasoningTokens:      uint64(u.CompletionTokensDetails.ReasoningTokens),
		ReasoningTokensKnown: reasoningKnown,
	}
}

// contentString turns Ark's ChatCompletionMessageContent, a string or a part list, into
// the plain string loom uses. A part list joins its text parts and ignores the rest, such
// as image, audio, or video parts, since loom's first version is text only.
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

// Keeps io.EOF from becoming an unused import: the provider does not use it directly, but
// StreamWithRetry inspects it with errors.Is when streamAdapter.Recv passes it up.
var _ = io.EOF

var _ = errors.As
