package openaicompat

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/respjson"

	"github.com/loomagent/loom"
)

// finishReason is the smallest legal Provider mapping: everything the shared code needs.
func finishReason(reason string) loom.FinishReason {
	switch reason {
	case "stop":
		return loom.FinishReasonStop
	case "tool_calls":
		return loom.FinishReasonToolCalls
	case "":
		return ""
	default:
		return loom.FinishReason(reason)
	}
}

// extraFields produces the SDK's view of a message's unknown fields, which is where a vendor
// puts its reasoning.
func extraFields(t *testing.T, raw string) map[string]respjson.Field {
	t.Helper()
	var message openai.ChatCompletionMessage
	if err := jsonv2.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatal(err)
	}
	return message.JSON.ExtraFields
}

func completion(t *testing.T, raw string) *openai.ChatCompletion {
	t.Helper()
	var out openai.ChatCompletion
	if err := jsonv2.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// A vendor that reports reasoning puts it in an extra field, and only a string is reasoning:
// a null or an object is not, and neither is a reason to fail the call over an optional
// field.
func TestReasoningContent(t *testing.T) {
	tests := []struct{ name, raw, want string }{
		{"structured", `{"reasoning_content":"why"}`, "why"},
		{"empty", `{"reasoning_content":""}`, ""},
		{"null", `{"reasoning_content":null}`, ""},
		{"object", `{"reasoning_content":{"step":1}}`, ""},
		{"absent", `{"content":"answer"}`, ""},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ReasoningContent(extraFields(t, testCase.raw)); got != testCase.want {
				t.Fatalf("ReasoningContent = %q, want %q", got, testCase.want)
			}
		})
	}
}

// An upstream provider may pass the generic field through instead, so it is accepted as a
// fallback — and only as a fallback, because the structured one is the vendor's own.
func TestReasoningContentOrReasoning(t *testing.T) {
	tests := []struct{ name, raw, want string }{
		{"structured wins", `{"reasoning_content":"structured","reasoning":"generic"}`, "structured"},
		{"generic alone", `{"reasoning":"generic"}`, "generic"},
		{"empty structured falls through", `{"reasoning_content":"","reasoning":"generic"}`, "generic"},
		{"generic is not a string", `{"reasoning":{"step":1}}`, ""},
		{"neither", `{"content":"answer"}`, ""},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ReasoningContentOrReasoning(extraFields(t, testCase.raw)); got != testCase.want {
				t.Fatalf("ReasoningContentOrReasoning = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestProviderResponse(t *testing.T) {
	provider := Provider{FinishReason: finishReason}
	out := completion(t, `{
		"model": "vendor-2026",
		"choices": [{
			"message": {
				"content": "answer", "reasoning_content": "why",
				"tool_calls": [{"id": "c1", "type": "function", "function": {"name": "lookup", "arguments": "{\"q\":\"x\"}"}}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7, "completion_tokens_details": {"reasoning_tokens": 2}}
	}`)

	response, err := provider.Response(out)
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "answer" || response.ReasoningContent != "why" || response.Model != "vendor-2026" {
		t.Fatalf("response = %+v", response)
	}
	if response.FinishReason != loom.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q", response.FinishReason)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].ID != "c1" || response.ToolCalls[0].Name != "lookup" {
		t.Fatalf("tool calls = %+v", response.ToolCalls)
	}
	if response.Usage.PromptTokens != 3 || response.Usage.TotalTokens != 7 || !response.Usage.ReasoningTokensKnown {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

// A response with nothing to read is an error rather than an empty answer, which would look
// like a model that chose to say nothing.
func TestProviderResponseRejectsWhatItCannotRead(t *testing.T) {
	provider := Provider{FinishReason: finishReason}
	if _, err := provider.Response(nil); err == nil {
		t.Fatal("a nil response must fail")
	}
	if _, err := provider.Response(completion(t, `{"choices":[]}`)); err == nil {
		t.Fatal("a response with no choices must fail")
	}
}

// A provider that refuses a finish reason aborts the call instead of mapping it.
func TestProviderResponseCanRefuseAFinishReason(t *testing.T) {
	refused := errors.New("sensitive content")
	provider := Provider{
		FinishReason: finishReason,
		CheckFinish: func(reason string) error {
			if reason == "sensitive" {
				return refused
			}
			return nil
		},
	}
	_, err := provider.Response(completion(t, `{"choices":[{"message":{"content":"x"},"finish_reason":"sensitive"}]}`))
	if !errors.Is(err, refused) {
		t.Fatalf("error = %v", err)
	}
	if _, err := provider.Response(completion(t, `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}]}`)); err != nil {
		t.Fatalf("an accepted finish reason failed: %v", err)
	}
}

// scriptedSource is a streaming reader a test drives frame by frame.
type scriptedSource struct {
	chunks []openai.ChatCompletionChunk
	index  int
	err    error
	closes int
}

func (s *scriptedSource) Next() bool {
	if s.index >= len(s.chunks) {
		return false
	}
	s.index++
	return true
}

func (s *scriptedSource) Current() openai.ChatCompletionChunk { return s.chunks[s.index-1] }
func (s *scriptedSource) Err() error                          { return s.err }
func (s *scriptedSource) Close() error                        { s.closes++; return nil }

func chunk(t *testing.T, raw string) openai.ChatCompletionChunk {
	t.Helper()
	var out openai.ChatCompletionChunk
	if err := jsonv2.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// drain reads a stream to its end and returns everything it produced.
func drain(t *testing.T, stream *Stream) (string, string, []loom.ToolCallDelta, loom.FinishReason, *loom.Usage, error) {
	t.Helper()
	var content, reasoning string
	var calls []loom.ToolCallDelta
	var finish loom.FinishReason
	var usage *loom.Usage
	for {
		chunk, err := stream.Recv()
		if err != nil {
			return content, reasoning, calls, finish, usage, err
		}
		content += chunk.ContentDelta
		reasoning += chunk.ReasoningContentDelta
		calls = append(calls, chunk.ToolCallDeltas...)
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
}

// The frames a streamed call arrives in, mapped the same way for every provider on this wire
// format.
func TestStreamMapsChunks(t *testing.T) {
	source := &scriptedSource{chunks: []openai.ChatCompletionChunk{
		chunk(t, `{"model":"vendor-2026","choices":[{"index":0,"delta":{"content":"he","reasoning_content":"th"}}]}`),
		chunk(t, `{"model":"vendor-2026","choices":[{"index":0,"delta":{"content":"llo","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`),
		chunk(t, `{"model":"vendor-2026","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`),
	}}
	stream := (Provider{FinishReason: finishReason}).Stream(source)

	content, reasoning, calls, finish, usage, err := drain(t, stream)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream = %v", err)
	}
	if content != "hello" || reasoning != "th" {
		t.Fatalf("content/reasoning = %q/%q", content, reasoning)
	}
	if finish != loom.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q", finish)
	}
	if len(calls) != 1 || calls[0].ID != "c1" || calls[0].Name != "lookup" {
		t.Fatalf("tool call deltas = %+v", calls)
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	if source.closes != 1 {
		t.Fatalf("source closed %d times", source.closes)
	}
}

// A stream that ends without a finish reason was cut off: reporting it as a clean end would
// make a partial answer look complete.
func TestStreamReportsATruncatedStream(t *testing.T) {
	source := &scriptedSource{chunks: []openai.ChatCompletionChunk{
		chunk(t, `{"choices":[{"index":0,"delta":{"content":"half"}}]}`),
	}}
	_, _, _, _, _, err := drain(t, (Provider{FinishReason: finishReason}).Stream(source))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("end of truncated stream = %v", err)
	}
}

// A provider that reports its own error type has it translated, because that is what the
// classifier above it reads.
func TestStreamReportsAndNormalizesTheSourceError(t *testing.T) {
	failure := errors.New("connection reset")
	source := &scriptedSource{err: failure}
	if _, _, _, _, _, err := drain(t, (Provider{FinishReason: finishReason}).Stream(source)); !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}

	normalized := errors.New("normalized")
	provider := Provider{
		FinishReason: finishReason,
		NormalizeError: func(error) error {
			return normalized
		},
	}
	if _, _, _, _, _, err := drain(t, provider.Stream(&scriptedSource{err: failure})); !errors.Is(err, normalized) {
		t.Fatalf("normalized error = %v", err)
	}
}

// Closing twice closes the source once, and a closed stream is at its end.
func TestStreamClose(t *testing.T) {
	source := &scriptedSource{chunks: []openai.ChatCompletionChunk{
		chunk(t, `{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`),
	}}
	stream := (Provider{FinishReason: finishReason}).Stream(source)
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if source.closes != 1 {
		t.Fatalf("source closed %d times", source.closes)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("recv after close = %v", err)
	}
}

// A refused finish reason aborts the stream rather than being mapped and handed on.
func TestStreamCanRefuseAFinishReason(t *testing.T) {
	refused := errors.New("network error")
	source := &scriptedSource{chunks: []openai.ChatCompletionChunk{
		chunk(t, `{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"network_error"}]}`),
	}}
	provider := Provider{
		FinishReason: finishReason,
		CheckFinish: func(reason string) error {
			if reason == "network_error" {
				return refused
			}
			return nil
		},
	}
	stream := provider.Stream(source)
	if _, err := stream.Recv(); !errors.Is(err, refused) {
		t.Fatalf("error = %v", err)
	}
	if source.closes != 1 {
		t.Fatalf("a refused finish reason must close the stream")
	}
}
