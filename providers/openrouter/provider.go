// Package openrouter implements loom.ChatModel on top of OpenRouter's
// OpenAI-compatible chat completions API, through github.com/openai/openai-go/v3.
//
// Where it differs from the native OpenAI protocol, which is why it is a package of its
// own rather than a reuse of deepseek:
//   - reasoning parameters: OpenRouter takes one request-level "reasoning" object,
//     {"enabled": bool, "effort": "low|medium|high"}, injected through SetExtraFields
//   - reasoning output: the message in a response or streaming delta carries a
//     "reasoning" field rather than deepseek's "reasoning_content", read from
//     ExtraFields
//
// Usage:
//
//	model, err := openrouter.New(openrouter.Config{
//	    APIKey:    os.Getenv("OPENROUTER_API_KEY"),
//	    ModelName: "x-ai/grok-4.3",
//	})
package openrouter

import (
	"context"
	jsonv2 "encoding/json/v2"
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

// DefaultBaseURL is the OpenRouter API endpoint.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// Config holds the OpenRouter provider's construction parameters.
type Config struct {
	// APIKey is required.
	APIKey string
	// ModelName is required: the OpenRouter model identifier, such as "x-ai/grok-4.3".
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

// Model is one OpenRouter model instance, implementing loom.ChatModel.
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
		return nil, fmt.Errorf("loom/openrouter: APIKey must not be empty")
	}
	if strings.TrimSpace(cfg.ModelName) == "" {
		return nil, fmt.Errorf("loom/openrouter: ModelName must not be empty")
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
		// loom owns every retry (ChatWithRetry / StreamWithRetry), so the SDK must
		// not retry underneath; its default retries would multiply the attempts and
		// bypass the shared rate-limit cooldown.
		client:       newClient(cfg, baseURL),
		name:         cfg.ModelName,
		retryCfg:     retryCfg,
		capabilities: capabilities,
	}, nil
}

// Name returns an identifier of the form "openrouter/<model>".
func (m *Model) Name() string {
	return "openrouter/" + m.name
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
		return nil, fmt.Errorf("loom/openrouter: chat: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("loom/openrouter: chat returned 0 choices")
	}
	choice := out.Choices[0]
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
// liveness probe. stream_options.include_usage is forced on, so the last frame carries
// Usage.
func (m *Model) Stream(ctx context.Context, req loom.ChatRequest) (loom.Stream, error) {
	orReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	orReq.StreamOptions.IncludeUsage = param.NewOpt(true)
	return loom.StreamWithRetry(ctx, classifier{}, m.retryCfg, func(streamCtx context.Context) (loom.Stream, error) {
		stream := m.client.Chat.Completions.NewStreaming(streamCtx, orReq)
		return &streamAdapter{inner: stream}, nil
	})
}

// streamAdapter wraps a go-openai ChatCompletionStream as a loom.Stream.
type streamAdapter struct {
	inner interface {
		Next() bool
		Current() openai.ChatCompletionChunk
		Err() error
		Close() error
	}
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
		u := openaicompat.Usage(&raw.Usage)
		chunk.Usage = &u
	}
	// The trailing include_usage frame has choices=[]; an ordinary frame's first delta is
	// taken.
	if len(raw.Choices) > 0 {
		choice := raw.Choices[0]
		chunk.ContentDelta = choice.Delta.Content
		chunk.ReasoningContentDelta = extractReasoning(choice.Delta.JSON.ExtraFields)
		chunk.ToolCallDeltas = openaicompat.ToolCallDeltas(choice.Delta.ToolCalls)
		if choice.FinishReason != "" {
			chunk.FinishReason = translateFinishReason(choice.FinishReason)
		}
	}
	return chunk, nil
}

func (s *streamAdapter) Close() error {
	return s.inner.Close()
}

// buildRequest translates a loom.ChatRequest into the go-openai request structure.
func (m *Model) buildRequest(req loom.ChatRequest) (_ openai.ChatCompletionNewParams, err error) {
	defer func() { err = loom.LocalRequestError(err) }()
	messages, err := translateMessages(req.Messages)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/openrouter: translate messages: %w", err)
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
	if len(req.Stop) == 1 {
		out.Stop.OfString = param.NewOpt(req.Stop[0])
	} else if len(req.Stop) > 1 {
		out.Stop.OfStringArray = req.Stop
	}

	if err := loom.CheckRequestAgainstCapabilities(m.capabilities, req); err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/openrouter: %w", err)
	}
	resolved, err := loom.ResolveModelReasoning("openrouter", m.name, m.capabilities, req.Reasoning)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/openrouter: %w", err)
	}
	reasoning, err := translateReasoning(resolved)
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	if reasoning != nil {
		out.SetExtraFields(map[string]any{"reasoning": reasoning})
	}

	if req.StructuredOutput != nil {
		switch req.StructuredOutput.Mode {
		case loom.StructuredOutputJSONSchema:
			out.ResponseFormat.OfJSONSchema = &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:        loom.NormalizeStructuredOutputName(req.StructuredOutput.Name),
					Description: param.NewOpt(req.StructuredOutput.Description),
					Schema:      req.StructuredOutput.Schema,
					// strict is always true: a provider's hard guarantee that the output conforms
					// is always what the caller wants; see the loom.StructuredOutput comment
					Strict: param.NewOpt(true),
				},
			}
		case loom.StructuredOutputJSONObject:
			out.ResponseFormat.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
		case loom.StructuredOutputUnsupported:
			// send nothing
		case loom.StructuredOutputNone:
			// The request side forbids none, which is reserved for capability declarations;
			// CheckRequestAgainstCapabilities already rejects it earlier
			return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/openrouter: StructuredOutput.Mode may not be %q", req.StructuredOutput.Mode)
		default:
			return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/openrouter: unknown structured output mode %q", req.StructuredOutput.Mode)
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
			// An unknown format sends nothing, matching the deepseek provider
		}
	}

	tools, err := openaicompat.Tools(req.Tools)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/openrouter: translate tools: %w", err)
	}
	out.Tools = tools
	if req.ToolChoice != nil {
		out.ToolChoice = openaicompat.ToolChoice(req.ToolChoice)
	}
	return out, nil
}

// translateReasoning turns a resolved decision into OpenRouter's single reasoning object.
// A nil return sends nothing, the omit case.
func translateReasoning(resolved loom.ResolvedReasoning) (map[string]any, error) {
	switch resolved.Send {
	case loom.ReasoningSendOmit:
		return nil, nil
	case loom.ReasoningSendDisabled:
		return map[string]any{"enabled": false}, nil
	case loom.ReasoningSendEnabled:
		reasoning := map[string]any{"enabled": true}
		if resolved.Effort != "" {
			reasoning["effort"] = string(resolved.Effort)
		}
		return reasoning, nil
	default:
		return nil, fmt.Errorf("loom/openrouter: unknown reasoning send %q", resolved.Send)
	}
}

// extractReasoning reads the reasoning output. OpenRouter uses a "reasoning" field in
// ExtraFields, but an upstream provider may pass through deepseek's structured
// reasoning_content instead, which takes precedence; otherwise "reasoning" is decoded
// from ExtraFields.
func extractReasoning(extra map[string]respjson.Field) string {
	if field, ok := extra["reasoning_content"]; ok {
		var s string
		if err := jsonv2.Unmarshal([]byte(field.Raw()), &s); err == nil && s != "" {
			return s
		}
	}
	raw, ok := extra["reasoning"]
	if !ok {
		return ""
	}
	var s string
	if err := jsonv2.Unmarshal([]byte(raw.Raw()), &s); err != nil {
		// "reasoning" is not a string, a null or an object for instance; ignore it
		return ""
	}
	return s
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
		// OpenRouter sometimes reports a value of its own, passed through from an upstream
		// provider; treat it as a natural stop
		return loom.FinishReasonStop
	}
}

// newClient builds the SDK client. Retries belong to loom, so the SDK must not retry
// underneath: its default retries would multiply the attempts and bypass the shared
// rate-limit cooldown.
func newClient(cfg Config, baseURL string) openai.Client {
	options := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithBaseURL(baseURL),
		option.WithMaxRetries(0),
	}
	if cfg.HTTPClient != nil {
		options = append(options, option.WithHTTPClient(cfg.HTTPClient))
	}
	return openai.NewClient(options...)
}
