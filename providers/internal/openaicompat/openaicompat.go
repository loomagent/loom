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
	jsonv2 "encoding/json/v2"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
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
	if u.JSON.CompletionTokensDetails.Valid() {
		out.ReasoningTokens = uint64(max(u.CompletionTokensDetails.ReasoningTokens, 0))
		out.ReasoningTokensKnown = u.CompletionTokensDetails.JSON.ReasoningTokens.Valid()
	}
	return out
}
