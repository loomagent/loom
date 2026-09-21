// Package openaicompat holds the chat-completions translation shared by the
// providers that speak the OpenAI wire format through
// github.com/openai/openai-go/v3: deepseek, openrouter, and zhipuai.
//
// Only translation that is identical across those providers lives here. A
// provider keeps whatever differs between them — request shape, extra fields,
// finish-reason defaults, reasoning extraction, usage fallbacks — so a
// divergence stays visible in the provider that has it instead of hiding behind
// a shared hook.
//
// The error text in Tools names the tool whose schema failed, so a caller can find
// which one it was.
package openaicompat

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/shared"

	"github.com/loomagent/loom"
)

// Tools translates declared tools into function tool parameters. A nil or empty
// slice means "no tools", which the API expresses by omitting the field.
func Tools(tools []*loom.ToolInfo) ([]openai.ChatCompletionToolUnionParam, error) {
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
				return nil, fmt.Errorf("tool %q argument schema could not be marshaled: %w", t.Name, err)
			}
			if err := jsonv2.Unmarshal(b, &params); err != nil {
				return nil, fmt.Errorf("tool %q argument schema could not be unmarshaled: %w", t.Name, err)
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

// ToolChoice translates the requested tool choice. An unknown mode leaves the
// field unset, so the server default applies rather than a guess.
func ToolChoice(tc *loom.ToolChoice) openai.ChatCompletionToolChoiceOptionUnionParam {
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
		return openai.ChatCompletionToolChoiceOptionUnionParam{}
	}
}

// ToolCalls translates the tool calls of a completed response. Calls that are
// not functions are dropped: Loom models function calling only.
func ToolCalls(calls []openai.ChatCompletionMessageToolCallUnion) []loom.ToolCall {
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

// ToolCallDeltas translates the tool-call increments of a streamed chunk.
func ToolCallDeltas(deltas []openai.ChatCompletionChunkChoiceDeltaToolCall) []loom.ToolCallDelta {
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

// Usage translates token accounting. ReasoningTokensKnown records whether the
// provider actually reported a reasoning count, which is not the same as
// reporting zero.
func Usage(u *openai.CompletionUsage) loom.Usage {
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
	// One vendor reports its cache-hit count under its own key rather than under
	// prompt_tokens_details.cached_tokens, so read that too when the standard field said
	// nothing. A provider that does not use the key never reaches this.
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

// The field an assistant turn's reasoning travels back in. The endpoints disagree about the
// name, not about whether they want it: a reasoning model that is not given its own previous
// reasoning back cannot continue the chain it started.
const (
	// ReasoningContentField is what DeepSeek and Zhipu call it.
	ReasoningContentField = "reasoning_content"
	// ReasoningField is what OpenRouter calls it.
	ReasoningField = "reasoning"
	// ReasoningDetailsField holds a provider's own structured reasoning: a sequence of typed
	// blocks it asks for back unchanged and in order.
	ReasoningDetailsField = "reasoning_details"
)

// ReasoningContent reads the structured reasoning field. A value that is not a string, a
// null or an object, is not reasoning, and an optional field that failed to arrive is not
// a reason to fail the call.
func ReasoningContent(extra map[string]respjson.Field) string {
	return stringField(extra, ReasoningContentField)
}

// ReasoningContentOrReasoning also accepts the generic reasoning field, which some upstream
// providers pass through instead of the structured one.
func ReasoningContentOrReasoning(extra map[string]respjson.Field) string {
	if text := ReasoningContent(extra); text != "" {
		return text
	}
	return stringField(extra, ReasoningField)
}

func stringField(extra map[string]respjson.Field, name string) string {
	field, ok := extra[name]
	if !ok {
		return ""
	}
	var text string
	if err := jsonv2.Unmarshal([]byte(field.Raw()), &text); err != nil {
		return ""
	}
	return text
}

// Provider carries what one provider does differently when a response is read back. The
// request shape stays with the provider, because that is where the endpoints disagree most;
// reading a completion and reading a stream are the same work for all of them.
type Provider struct {
	// FinishReason maps the provider's own finish reason onto Loom's. Required.
	FinishReason func(string) loom.FinishReason
	// Reasoning reads the provider's reasoning out of a message's extra fields. The default
	// reads the structured field.
	Reasoning func(map[string]respjson.Field) string
	// CheckFinish, when set, refuses a finish reason the provider rejects outright instead
	// of mapping it.
	CheckFinish func(string) error
	// NormalizeError, when set, translates an SDK error into the provider's own, which is
	// what the provider's classifier and its callers expect to see.
	NormalizeError func(error) error

	// ReasoningField names the field an assistant turn's reasoning text travels back in, or
	// "" for a provider that carries none.
	ReasoningField string
	// ReasoningDetailsField names the field its structured reasoning travels in, or "" for a
	// provider that has no such form.
	ReasoningDetailsField string
}

func (p Provider) normalize(err error) error {
	if p.NormalizeError == nil {
		return err
	}
	return p.NormalizeError(err)
}

func (p Provider) reasoning() func(map[string]respjson.Field) string {
	if p.Reasoning != nil {
		return p.Reasoning
	}
	return ReasoningContent
}

// Response maps one completed call onto Loom's response. A call with no choices is an error
// rather than an empty answer: every provider can return one, and reporting it as no content
// would look like a model that chose to say nothing.
func (p Provider) Response(out *openai.ChatCompletion) (*loom.ChatResponse, error) {
	if out == nil || len(out.Choices) == 0 {
		return nil, errors.New("the response has no choices")
	}
	choice := out.Choices[0]
	if p.CheckFinish != nil {
		if err := p.CheckFinish(choice.FinishReason); err != nil {
			return nil, err
		}
	}
	return &loom.ChatResponse{
		Content:          choice.Message.Content,
		ReasoningContent: p.reasoning()(choice.Message.JSON.ExtraFields),
		ReasoningDetails: detailsField(choice.Message.JSON.ExtraFields, p.ReasoningDetailsField),
		ToolCalls:        ToolCalls(choice.Message.ToolCalls),
		FinishReason:     p.FinishReason(choice.FinishReason),
		Usage:            Usage(&out.Usage),
		Model:            out.Model,
	}, nil
}

// detailsField reads a provider's structured reasoning as the raw JSON it arrived as, because
// it has to go back the way it came.
func detailsField(extra map[string]respjson.Field, name string) jsontext.Value {
	if name == "" {
		return nil
	}
	field, ok := extra[name]
	if !ok {
		return nil
	}
	return jsontext.Value(field.Raw())
}

// StreamSource is the SDK's streaming reader. Every OpenAI-wire SDK spells it the same way.
type StreamSource interface {
	Next() bool
	Current() openai.ChatCompletionChunk
	Err() error
	Close() error
}

// Stream adapts a chat-completion stream to loom.Stream.
type Stream struct {
	source   StreamSource
	provider Provider
	closed   bool
	finished bool
}

// Stream wraps source as a Loom stream.
func (p Provider) Stream(source StreamSource) *Stream {
	return &Stream{source: source, provider: p}
}

// Recv returns the next chunk.
//
// A stream that ends without a finish reason is reported as truncated rather than as a clean
// end: the endpoint always sends one, so its absence means the answer was cut off.
func (s *Stream) Recv() (*loom.Chunk, error) {
	if s.closed {
		return nil, io.EOF
	}
	if !s.source.Next() {
		defer func() { _ = s.Close() }() // Keep the stream's own error; closing is cleanup.
		if err := s.source.Err(); err != nil {
			return nil, s.provider.normalize(err)
		}
		if !s.finished {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, io.EOF
	}
	raw := s.source.Current()

	chunk := &loom.Chunk{Model: raw.Model}
	if raw.JSON.Usage.Valid() {
		usage := Usage(&raw.Usage)
		chunk.Usage = &usage
	}
	// The trailing usage-only frame has choices=[]; an ordinary frame's first delta is taken.
	if len(raw.Choices) > 0 {
		choice := raw.Choices[0]
		chunk.ContentDelta = choice.Delta.Content
		chunk.ReasoningContentDelta = s.provider.reasoning()(choice.Delta.JSON.ExtraFields)
		chunk.ReasoningDetails = detailsField(choice.Delta.JSON.ExtraFields, s.provider.ReasoningDetailsField)
		chunk.ToolCallDeltas = ToolCallDeltas(choice.Delta.ToolCalls)
		if choice.FinishReason != "" {
			if s.provider.CheckFinish != nil {
				if err := s.provider.CheckFinish(choice.FinishReason); err != nil {
					_ = s.Close()
					return nil, err
				}
			}
			s.finished = true
			chunk.FinishReason = s.provider.FinishReason(choice.FinishReason)
		}
	}
	return chunk, nil
}

// Close closes the source once. A second call does nothing.
func (s *Stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.source.Close()
}

// Messages translates Loom messages onto the OpenAI wire format.
//
// An assistant turn's reasoning travels back in the fields this provider names, because a
// reasoning model that is not handed its own previous reasoning cannot continue the chain it
// started. The text form is the one the endpoints spell differently; the structured form is a
// provider's own sequence of blocks, which goes back exactly as it arrived.
//
// An unknown role is an error rather than a user message: quietly changing who said something
// rewrites the conversation, and nothing downstream would notice.
func (p Provider) Messages(msgs []loom.Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		var gm openai.ChatCompletionMessageParamUnion
		switch m.Role {
		case loom.RoleSystem:
			gm = openai.SystemMessage(m.Content)
		case loom.RoleAssistant:
			gm = openai.AssistantMessage(m.Content)
			extra := map[string]any{}
			if m.ReasoningContent != "" && p.ReasoningField != "" {
				extra[p.ReasoningField] = m.ReasoningContent
			}
			// The structured form goes back verbatim, and a provider that does not carry one
			// never sees the field.
			if len(m.ReasoningDetails) > 0 && p.ReasoningDetailsField != "" {
				extra[p.ReasoningDetailsField] = m.ReasoningDetails
			}
			if len(extra) > 0 {
				gm.OfAssistant.SetExtraFields(extra)
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

// Client builds an SDK client for one provider. Retries belong to Loom, so the SDK must not
// retry underneath: its default retries would multiply the attempts and bypass the shared
// rate-limit cooldown.
func Client(apiKey, baseURL string, httpClient *http.Client) openai.Client {
	options := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithMaxRetries(0),
	}
	if httpClient != nil {
		options = append(options, option.WithHTTPClient(httpClient))
	}
	return openai.NewClient(options...)
}
