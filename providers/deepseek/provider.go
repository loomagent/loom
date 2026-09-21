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
// Usage:
//
//	model, err := deepseek.New(deepseek.Config{
//	    APIKey:    os.Getenv("DEEPSEEK_API_KEY"),
//	    ModelName: deepseek.ModelV4Flash,
//	})
//	if err != nil { ... }
//
//	resp, err := model.Chat(ctx, loom.ChatRequest{
//	    Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hello"}},
//	    Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: loom.ReasoningEffortHigh},
//	})
package deepseek

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/internal/openaicompat"
)

// DefaultBaseURL is the DeepSeek API endpoint.
const DefaultBaseURL = "https://api.deepseek.com"

// The default model aliases.
const (
	ModelV4Flash = "deepseek-v4-flash"
	ModelV4Pro   = "deepseek-v4-pro"
)

// Config holds the DeepSeek provider's construction parameters.
type Config struct {
	// APIKey is required.
	APIKey string
	// ModelName is required, such as ModelV4Flash or ModelV4Pro; empty means
	// ModelV4Flash.
	ModelName string
	// BaseURL may be empty, in which case DefaultBaseURL is used.
	BaseURL string

	// Retry controls the retry policy; nil means loom.DefaultRetryConfig(), which retries
	// by default. To turn retries off entirely, pass &loom.RetryConfig{MaxRetries: -1}:
	// a MaxRetries below zero gives up after one attempt at any Transient error. A
	// RateLimit still retries without end, which is the provider-side throttling
	// backstop, and product code should not disable it.
	Retry *loom.RetryConfig
	// HTTPClient is optional, and replaces the client every request goes through. Use it
	// for a proxy, custom timeouts, or a test server's in-memory client.
	HTTPClient *http.Client

	// Capabilities is what the caller or modelfactory fills in from the model's real
	// configuration. nil leaves it undeclared, so capability checks pass requests
	// through.
	Capabilities *loom.ModelCapabilities
}

// Model is one DeepSeek model instance, implementing loom.ChatModel.
type Model struct {
	client       openai.Client
	name         string
	retryCfg     *loom.RetryConfig
	capabilities loom.ModelCapabilities
}

// Compile-time interface check.
var _ loom.ChatModel = (*Model)(nil)

// New builds a Model.
func New(cfg Config) (*Model, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("loom/deepseek: APIKey must not be empty")
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
		client:       openaicompat.Client(cfg.APIKey, baseURL, cfg.HTTPClient),
		name:         name,
		retryCfg:     retryCfg,
		capabilities: capabilities,
	}, nil
}

// Name returns an identifier of the form "deepseek/<model>".
func (m *Model) Name() string {
	return "deepseek/" + m.name
}

// Capabilities returns the model capabilities declared at initialization.
func (m *Model) Capabilities() loom.ModelCapabilities {
	return m.capabilities
}

// Chat implements loom.ChatModel.Chat with automatic retries. m.retryCfg controls the
// policy; see Config.Retry. This provider's classifier{} supplies the error
// classification from status code to ErrorClass.
func (m *Model) Chat(ctx context.Context, req loom.ChatRequest) (*loom.ChatResponse, error) {
	dsReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	return loom.ChatWithRetry(ctx, classifier{}, m.retryCfg, func(callCtx context.Context) (*loom.ChatResponse, error) {
		return m.chatRaw(callCtx, dsReq)
	})
}

// chatRaw is one synchronous Chat call with no retry, which the retry helper calls
// repeatedly.
func (m *Model) chatRaw(ctx context.Context, dsReq openai.ChatCompletionNewParams) (*loom.ChatResponse, error) {
	out, err := m.client.Chat.Completions.New(ctx, dsReq)
	if err != nil {
		return nil, fmt.Errorf("loom/deepseek: chat: %w", normalizeDeepSeekError(err))
	}
	response, err := wire.Response(out)
	if err != nil {
		return nil, fmt.Errorf("loom/deepseek: chat: %w", err)
	}
	return response, nil
}

// wire is what this provider does differently when a response is read back. The request
// shape stays here; reading a completion and reading a stream are the same work for every
// provider on this wire format.
var wire = openaicompat.Provider{FinishReason: translateFinishReason}

// Stream implements loom.ChatModel.Stream with automatic retries, up to the first-frame
// liveness probe. Once the consumer has the second frame or later, there are no more
// retries. stream_options.include_usage is forced on by default, so the last frame
// carries Usage.
func (m *Model) Stream(ctx context.Context, req loom.ChatRequest) (loom.Stream, error) {
	dsReq, err := m.buildRequest(req)
	if err != nil {
		return nil, err
	}
	dsReq.StreamOptions.IncludeUsage = param.NewOpt(true)
	return loom.StreamWithRetry(ctx, classifier{}, m.retryCfg, func(streamCtx context.Context) (loom.Stream, error) {
		return wire.Stream(m.client.Chat.Completions.NewStreaming(streamCtx, dsReq)), nil
	})
}

// buildRequest validates explicit configuration and serializes the requested
// protocol fields. It does not infer capabilities from the provider/model name.
func (m *Model) buildRequest(req loom.ChatRequest) (_ openai.ChatCompletionNewParams, err error) {
	defer func() { err = loom.LocalRequestError(err) }()
	messages, err := openaicompat.Messages(req.Messages, openaicompat.ReasoningContentField)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: translate messages: %w", err)
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
	switch len(req.Stop) {
	case 0:
		// send nothing
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
	// thinking is a DeepSeek-specific field with no typed place in the SDK, so it is
	// injected as needed. ReasoningSendOmit injects nothing, which is the same as leaving
	// the field out.
	switch resolved.Send {
	case loom.ReasoningSendEnabled:
		out.SetExtraFields(map[string]any{"thinking": map[string]any{"type": "enabled"}})
	case loom.ReasoningSendDisabled:
		out.SetExtraFields(map[string]any{"thinking": map[string]any{"type": "disabled"}})
	case loom.ReasoningSendOmit:
		// send no thinking field
	default:
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: unknown reasoning send %q", resolved.Send)
	}
	out.ReasoningEffort = shared.ReasoningEffort(resolved.Effort)

	if req.StructuredOutput != nil {
		switch req.StructuredOutput.Mode {
		case loom.StructuredOutputJSONObject:
			out.ResponseFormat.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
		case loom.StructuredOutputJSONSchema:
			if req.StructuredOutput.Schema == nil {
				return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: json_schema structured output has no schema")
			}
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
		case loom.StructuredOutputUnsupported:
			// send nothing
		case loom.StructuredOutputNone:
			// The request side forbids none, which is reserved for capability declarations;
			// CheckRequestAgainstCapabilities already rejects it earlier
			return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: StructuredOutput.Mode may not be %q", req.StructuredOutput.Mode)
		default:
			return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: unknown structured output mode %q", req.StructuredOutput.Mode)
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
			// An unknown format sends nothing
		}
	}
	tools, err := openaicompat.Tools(req.Tools)
	if err != nil {
		return openai.ChatCompletionNewParams{}, fmt.Errorf("loom/deepseek: translate tools: %w", err)
	}
	out.Tools = tools
	if req.ToolChoice != nil {
		out.ToolChoice = openaicompat.ToolChoice(req.ToolChoice)
	}
	return out, nil
}

// normalizeDeepSeekError maps DeepSeek's own business errors onto loom's sentinels.
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
