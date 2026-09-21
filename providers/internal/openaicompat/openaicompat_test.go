package openaicompat

import (
	jsonv2 "encoding/json/v2"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/loomagent/loom"
)

func TestTools(t *testing.T) {
	if tools, err := Tools(nil); err != nil || tools != nil {
		t.Fatalf("nil tools = %v (%v)", tools, err)
	}

	tools, err := Tools([]*loom.ToolInfo{
		nil,
		{Name: "search", Description: "Search.", Parameters: &loom.Schema{
			Type:       "object",
			Properties: map[string]*loom.Schema{"q": {Type: "string"}},
		}},
		{Name: "plain", Description: "No parameters."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2: a nil entry is skipped, not an error", len(tools))
	}
	first := tools[0].GetFunction()
	if first == nil || first.Name != "search" || first.Parameters["type"] != "object" {
		t.Fatalf("tool = %+v", tools[0])
	}
	second := tools[1].GetFunction()
	properties, ok := second.Parameters["properties"].(map[string]any)
	if !ok || len(properties) != 0 {
		t.Fatalf("a tool without parameters must still declare an empty object: %+v", second.Parameters)
	}
}

// A schema that cannot be serialized is a caller mistake, and the error names
// the tool that carries it.
func TestToolsRejectsUnserializableSchema(t *testing.T) {
	_, err := Tools([]*loom.ToolInfo{{Name: "broken", Parameters: &loom.Schema{Enum: []any{make(chan int)}}}})
	if err == nil {
		t.Fatal("an unserializable schema must fail")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("error does not name the tool: %v", err)
	}
}

func TestToolChoice(t *testing.T) {
	if got := ToolChoice(nil); got.OfAuto.Valid() {
		t.Fatalf("nil tool choice = %+v", got)
	}
	for mode, want := range map[loom.ToolChoiceMode]string{
		loom.ToolChoiceAuto:     "auto",
		loom.ToolChoiceNone:     "none",
		loom.ToolChoiceRequired: "required",
	} {
		choice := ToolChoice(&loom.ToolChoice{Mode: mode})
		if choice.OfAuto.Value != want {
			t.Errorf("mode %q mapped to %q, want %q", mode, choice.OfAuto.Value, want)
		}
	}
	got := ToolChoice(&loom.ToolChoice{Mode: loom.ToolChoiceSpecific, Name: "lookup"})
	if got.OfFunctionToolChoice == nil || got.OfFunctionToolChoice.Function.Name != "lookup" {
		t.Errorf("specific choice = %+v", got)
	}
	// An unknown mode is not sent at all, leaving the provider default in place.
	if got := ToolChoice(&loom.ToolChoice{Mode: loom.ToolChoiceMode("unknown")}); got.OfAuto.Valid() || got.OfFunctionToolChoice != nil {
		t.Errorf("unknown mode sent %+v", got)
	}
}

// Only function calls are modelled, so any other tool call is dropped rather
// than translated into a call the runtime cannot dispatch.
func TestToolCalls(t *testing.T) {
	if got := ToolCalls(nil); got != nil {
		t.Fatalf("nil calls = %+v", got)
	}
	got := ToolCalls([]openai.ChatCompletionMessageToolCallUnion{
		{ID: "1", Type: "function", Function: openai.ChatCompletionMessageFunctionToolCallFunction{Name: "search", Arguments: `{"q":"x"}`}},
		{ID: "2", Type: "custom"},
	})
	if len(got) != 1 {
		t.Fatalf("calls = %+v, want only the function call", got)
	}
	if got[0].ID != "1" || got[0].Name != "search" || got[0].Arguments != `{"q":"x"}` {
		t.Fatalf("call = %+v", got[0])
	}
}

func TestToolCallDeltas(t *testing.T) {
	if got := ToolCallDeltas(nil); got != nil {
		t.Fatalf("nil deltas = %+v", got)
	}
	got := ToolCallDeltas([]openai.ChatCompletionChunkChoiceDeltaToolCall{
		{Index: 2, ID: "1", Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{Name: "search", Arguments: `{"q":`}},
	})
	if len(got) != 1 {
		t.Fatalf("deltas = %+v", got)
	}
	if got[0].Index != 2 || got[0].ID != "1" || got[0].Name != "search" || got[0].Arguments != `{"q":` {
		t.Fatalf("delta = %+v", got[0])
	}
}

// ReasoningTokensKnown separates "the provider reported zero" from "the provider
// did not report at all", which a probe report has to keep apart.
func TestUsage(t *testing.T) {
	if got := Usage(nil); got != (loom.Usage{}) {
		t.Fatalf("nil usage = %+v", got)
	}

	var usage openai.CompletionUsage
	if err := jsonv2.Unmarshal([]byte(`{
		"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3,
		"prompt_tokens_details": {"cached_tokens": 4},
		"completion_tokens_details": {"reasoning_tokens": 5}
	}`), &usage); err != nil {
		t.Fatal(err)
	}
	got := Usage(&usage)
	if got.PromptTokens != 1 || got.CompletionTokens != 2 || got.TotalTokens != 3 || got.CachedTokens != 4 {
		t.Fatalf("usage = %+v", got)
	}
	if got.ReasoningTokens != 5 || !got.ReasoningTokensKnown {
		t.Fatalf("reasoning presence = %+v", got)
	}

	var bare openai.CompletionUsage
	if err := jsonv2.Unmarshal([]byte(`{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}`), &bare); err != nil {
		t.Fatal(err)
	}
	if got := Usage(&bare); got.ReasoningTokensKnown || got.CachedTokens != 0 {
		t.Fatalf("absent details = %+v", got)
	}

	var negative openai.CompletionUsage
	if err := jsonv2.Unmarshal([]byte(`{"prompt_tokens": -1, "completion_tokens": -2, "total_tokens": -3}`), &negative); err != nil {
		t.Fatal(err)
	}
	if got := Usage(&negative); got.PromptTokens != 0 || got.CompletionTokens != 0 || got.TotalTokens != 0 {
		t.Fatalf("negative counts became %+v", got)
	}

	// A reasoning count that is present and zero is not the same as one that never arrived.
	var explicitZero openai.CompletionUsage
	if err := jsonv2.Unmarshal([]byte(`{"completion_tokens_details":{"reasoning_tokens":0}}`), &explicitZero); err != nil {
		t.Fatal(err)
	}
	if got := Usage(&explicitZero); !got.ReasoningTokensKnown || got.ReasoningTokens != 0 {
		t.Fatalf("explicit zero = %+v", got)
	}
	var explicitNull openai.CompletionUsage
	if err := jsonv2.Unmarshal([]byte(`{"completion_tokens_details":{"reasoning_tokens":null}}`), &explicitNull); err != nil {
		t.Fatal(err)
	}
	if got := Usage(&explicitNull); got.ReasoningTokensKnown {
		t.Fatalf("a null count reported as known = %+v", got)
	}

	// One vendor reports its cache-hit count under its own key, which is read only when the
	// standard field said nothing.
	var vendorCache openai.CompletionUsage
	if err := jsonv2.Unmarshal([]byte(`{"prompt_tokens":9,"prompt_cache_hit_tokens":4}`), &vendorCache); err != nil {
		t.Fatal(err)
	}
	if got := Usage(&vendorCache); got.CachedTokens != 4 {
		t.Fatalf("vendor cache key = %+v", got)
	}
	// A standard count wins over the vendor key.
	var both openai.CompletionUsage
	if err := jsonv2.Unmarshal([]byte(`{"prompt_tokens_details":{"cached_tokens":7},"prompt_cache_hit_tokens":4}`), &both); err != nil {
		t.Fatal(err)
	}
	if got := Usage(&both); got.CachedTokens != 7 {
		t.Fatalf("standard cache count = %+v", got)
	}
}
