package ark

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"

	"github.com/loomagent/loom"
)

// Every mapping below is a function with no error return: a wrong branch does not fail a
// call, it hands loom the wrong value. So each one is pinned here, case by case.

func TestTranslateFinishReason(t *testing.T) {
	for raw, want := range map[arkmodel.FinishReason]loom.FinishReason{
		arkmodel.FinishReasonStop:          loom.FinishReasonStop,
		arkmodel.FinishReasonLength:        loom.FinishReasonLength,
		arkmodel.FinishReasonContentFilter: loom.FinishReasonContentFilter,
		arkmodel.FinishReasonToolCalls:     loom.FinishReasonToolCalls,
		arkmodel.FinishReasonFunctionCall:  loom.FinishReasonToolCalls,
		arkmodel.FinishReasonNull:          "",
		"":                                 "",
		// An unrecognised value passes through rather than being dropped, so the caller
		// still sees what the provider said.
		"something_new": loom.FinishReason("something_new"),
	} {
		if got := translateFinishReason(raw); got != want {
			t.Errorf("translateFinishReason(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestTranslateToolChoice(t *testing.T) {
	if got := translateToolChoice(nil); got != nil {
		t.Fatalf("nil tool choice = %v", got)
	}
	for mode, want := range map[loom.ToolChoiceMode]string{
		loom.ToolChoiceAuto:     "auto",
		loom.ToolChoiceNone:     "none",
		loom.ToolChoiceRequired: "required",
	} {
		if got := translateToolChoice(&loom.ToolChoice{Mode: mode}); got != want {
			t.Errorf("mode %q mapped to %v, want %q", mode, got, want)
		}
	}
	specific := translateToolChoice(&loom.ToolChoice{Mode: loom.ToolChoiceSpecific, Name: "lookup"})
	named, ok := specific.(map[string]any)
	if !ok {
		t.Fatalf("specific choice = %#v", specific)
	}
	function, ok := named["function"].(map[string]string)
	if named["type"] != "function" || !ok || function["name"] != "lookup" {
		t.Fatalf("specific choice = %#v", specific)
	}
	// An unknown mode is not sent at all, so the server default applies.
	if got := translateToolChoice(&loom.ToolChoice{Mode: loom.ToolChoiceMode("unknown")}); got != nil {
		t.Errorf("unknown mode sent %v", got)
	}
}

func TestTranslateRole(t *testing.T) {
	for role, want := range map[loom.Role]string{
		loom.RoleSystem:    arkmodel.ChatMessageRoleSystem,
		loom.RoleUser:      arkmodel.ChatMessageRoleUser,
		loom.RoleAssistant: arkmodel.ChatMessageRoleAssistant,
		loom.RoleTool:      arkmodel.ChatMessageRoleTool,
	} {
		if got := translateRole(role); got != want {
			t.Errorf("translateRole(%q) = %q, want %q", role, got, want)
		}
	}
	if got := translateRole(loom.Role("custom")); got != "custom" {
		t.Errorf("unknown role = %q", got)
	}
}

func TestTranslateTools(t *testing.T) {
	tools, err := translateTools(nil)
	if err != nil || len(tools) != 0 {
		t.Fatalf("nil tools = %v (%v)", tools, err)
	}

	tools, err = translateTools([]*loom.ToolInfo{
		nil,
		{
			Name:        "search",
			Description: "Search.",
			Parameters:  &loom.Schema{Type: "object", Properties: map[string]*loom.Schema{"q": {Type: "string"}}},
		},
		{Name: "plain", Description: "No parameters."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2: a nil entry is skipped", len(tools))
	}
	if tools[0].Type != arkmodel.ToolTypeFunction || tools[0].Function.Name != "search" {
		t.Fatalf("tool = %+v", tools[0])
	}
	schema, ok := tools[0].Function.Parameters.(jsontext.Value)
	if !ok || !strings.Contains(string(schema), `"q"`) {
		t.Fatalf("parameter schema was dropped: %#v", tools[0].Function.Parameters)
	}
	if empty, ok := tools[1].Function.Parameters.(jsontext.Value); !ok || len(empty) != 0 {
		t.Fatalf("a tool without parameters carries %#v", tools[1].Function.Parameters)
	}
}

// A schema that cannot be serialized must fail the request. Advertising the tool without
// its parameters would let the model call it with unconstrained arguments, and nothing
// downstream would notice.
func TestTranslateToolsRejectsUnserializableSchema(t *testing.T) {
	_, err := translateTools([]*loom.ToolInfo{{
		Name:       "broken",
		Parameters: &loom.Schema{Enum: []any{make(chan int)}},
	}})
	if err == nil {
		t.Fatal("an unserializable schema must fail")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("error does not name the tool: %v", err)
	}
}

func TestTranslateToolCalls(t *testing.T) {
	if got := translateToolCalls(nil); got != nil {
		t.Fatalf("nil calls = %+v", got)
	}
	got := translateToolCalls([]*arkmodel.ToolCall{
		nil,
		{ID: "c1", Type: arkmodel.ToolTypeFunction, Function: arkmodel.FunctionCall{Name: "search", Arguments: `{"q":"x"}`}},
	})
	if len(got) != 1 {
		t.Fatalf("calls = %+v, want only the real one", got)
	}
	if got[0].ID != "c1" || got[0].Name != "search" || got[0].Arguments != `{"q":"x"}` {
		t.Fatalf("call = %+v", got[0])
	}
}

func TestTranslateToolCallDeltas(t *testing.T) {
	if got := translateToolCallDeltas(nil); got != nil {
		t.Fatalf("nil deltas = %+v", got)
	}
	index := 2
	got := translateToolCallDeltas([]*arkmodel.ToolCall{
		nil,
		{ID: "c1", Function: arkmodel.FunctionCall{Name: "search", Arguments: `{"q":`}, Index: &index},
		{ID: "c2", Function: arkmodel.FunctionCall{Arguments: `"x"}`}},
	})
	if len(got) != 2 {
		t.Fatalf("deltas = %+v", got)
	}
	if got[0].Index != 2 || got[0].ID != "c1" || got[0].Name != "search" || got[0].Arguments != `{"q":` {
		t.Fatalf("delta = %+v", got[0])
	}
	// A frame without an index belongs to the call opened first.
	if got[1].Index != 0 || got[1].Arguments != `"x"}` {
		t.Fatalf("delta without index = %+v", got[1])
	}
}

func TestContentString(t *testing.T) {
	if got := contentString(nil); got != "" {
		t.Fatalf("nil content = %q", got)
	}
	text := "plain"
	if got := contentString(&arkmodel.ChatCompletionMessageContent{StringValue: &text}); got != "plain" {
		t.Fatalf("string content = %q", got)
	}
	partList := &arkmodel.ChatCompletionMessageContent{ListValue: []*arkmodel.ChatCompletionMessageContentPart{
		nil,
		{Type: arkmodel.ChatCompletionMessageContentPartTypeText, Text: "first "},
		{Type: arkmodel.ChatCompletionMessageContentPartTypeImageURL, ImageURL: &arkmodel.ChatMessageImageURL{URL: "http://x"}},
		{Type: arkmodel.ChatCompletionMessageContentPartTypeText, Text: "second"},
	}}
	if got := contentString(partList); got != "first second" {
		t.Fatalf("part list = %q", got)
	}
}

func TestApplyResponseFormat(t *testing.T) {
	schema := &loom.Schema{Type: "object"}
	tests := []struct {
		name    string
		req     loom.ChatRequest
		want    arkmodel.ResponseFormatType
		wantErr string
	}{
		{name: "json_schema", req: loom.ChatRequest{StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputJSONSchema, Name: "Result Name", Schema: schema}}, want: arkmodel.ResponseFormatJSONSchema},
		{name: "json_schema without a schema", req: loom.ChatRequest{StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputJSONSchema, Name: "r"}}, wantErr: "has no schema"},
		{name: "json_object", req: loom.ChatRequest{StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputJSONObject}}, want: arkmodel.ResponseFormatJsonObject},
		{name: "unsupported sends nothing", req: loom.ChatRequest{StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputUnsupported}}},
		{name: "none is rejected on the request side", req: loom.ChatRequest{StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputNone}}, wantErr: "may not be"},
		{name: "unknown mode", req: loom.ChatRequest{StructuredOutput: &loom.StructuredOutput{Mode: loom.StructuredOutputMode("other")}}, wantErr: "unknown structured output mode"},
		{name: "response format json_object", req: loom.ChatRequest{ResponseFormat: loom.ResponseFormatJSONObject}, want: arkmodel.ResponseFormatJsonObject},
		{name: "response format text", req: loom.ChatRequest{ResponseFormat: loom.ResponseFormatText}, want: arkmodel.ResponseFormatText},
		{name: "response format default", req: loom.ChatRequest{ResponseFormat: loom.ResponseFormatDefault}},
		{name: "unknown response format", req: loom.ChatRequest{ResponseFormat: loom.ResponseFormat("other")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out arkmodel.CreateChatCompletionRequest
			err := applyResponseFormat(&out, tt.req)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if out.ResponseFormat != nil {
					t.Fatalf("response format = %+v", out.ResponseFormat)
				}
				return
			}
			if out.ResponseFormat == nil || out.ResponseFormat.Type != tt.want {
				t.Fatalf("response format = %+v", out.ResponseFormat)
			}
			// The schema name goes out as the endpoint accepts it, and strict is not a switch.
			if tt.want == arkmodel.ResponseFormatJSONSchema {
				if out.ResponseFormat.JSONSchema.Name != "Result_Name" || !out.ResponseFormat.JSONSchema.Strict {
					t.Fatalf("json_schema = %+v", out.ResponseFormat.JSONSchema)
				}
			}
		})
	}
}

func TestTranslateMessages(t *testing.T) {
	text := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	msgs := translateMessages([]loom.Message{
		{Role: loom.RoleSystem, Parts: []loom.ContentPart{loom.TextPart("rules")}},
		{Role: loom.RoleUser, Parts: []loom.ContentPart{loom.TextPart("hi")}},
		{
			Role:             loom.RoleAssistant,
			Parts:            []loom.ContentPart{loom.TextPart("calling")},
			ReasoningContent: "why",
			Name:             "assistant",
			ToolCalls:        []loom.ToolCall{{ID: "c1", Name: "search", Arguments: `{"q":"x"}`}},
		},
		{Role: loom.RoleAssistant, ToolCalls: []loom.ToolCall{{ID: "c2", Name: "lookup"}}},
		{Role: loom.RoleTool, Parts: []loom.ContentPart{loom.TextPart("result")}, ToolCallID: "c1"},
	})
	if len(msgs) != 5 {
		t.Fatalf("messages = %d", len(msgs))
	}
	if msgs[0].Role != arkmodel.ChatMessageRoleSystem || msgs[1].Role != arkmodel.ChatMessageRoleUser {
		t.Fatalf("roles = %q, %q", msgs[0].Role, msgs[1].Role)
	}
	assistant := msgs[2]
	if assistant.Role != arkmodel.ChatMessageRoleAssistant || text(assistant.Content.StringValue) != "calling" {
		t.Fatalf("assistant = %+v", assistant)
	}
	if assistant.ReasoningContent == nil || *assistant.ReasoningContent != "why" || assistant.Name == nil || *assistant.Name != "assistant" {
		t.Fatalf("assistant evidence = %+v", assistant)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "c1" ||
		assistant.ToolCalls[0].Function.Name != "search" || assistant.ToolCalls[0].Function.Arguments != `{"q":"x"}` {
		t.Fatalf("assistant tool calls = %+v", assistant.ToolCalls)
	}
	// An assistant message that only calls tools carries no content at all, rather than an
	// empty string the model would read as an answer.
	if calls := msgs[3]; calls.Content != nil || len(calls.ToolCalls) != 1 {
		t.Fatalf("tool-only assistant = %+v", calls)
	}
	if tool := msgs[4]; tool.Role != arkmodel.ChatMessageRoleTool ||
		tool.ToolCallID != "c1" || text(tool.Content.StringValue) != "result" {
		t.Fatalf("tool message = %+v", tool)
	}
}

// A user message that carries an image goes out as the part list, in the order the caller gave
// the parts. Every other message, and a user message of text only, stays the plain string. An
// image's detail is sent only when the caller set one, so the endpoint keeps its default.
func TestTranslateMessagesMultimodal(t *testing.T) {
	const cat = "https://example.com/cat.png"
	msgs := translateMessages([]loom.Message{
		{Role: loom.RoleSystem, Parts: []loom.ContentPart{loom.TextPart("rules")}},
		{Role: loom.RoleUser, Parts: []loom.ContentPart{loom.TextPart("What is in this image?"), loom.ImagePart(cat, "")}},
		{Role: loom.RoleUser, Parts: []loom.ContentPart{loom.ImagePart(cat, "high")}},
		{Role: loom.RoleUser, Parts: []loom.ContentPart{loom.TextPart("plain")}},
	})
	if len(msgs) != 4 {
		t.Fatalf("messages = %d", len(msgs))
	}
	// Text only, on any role, is the string form.
	for _, i := range []int{0, 3} {
		if c := msgs[i].Content; c == nil || c.StringValue == nil || c.ListValue != nil {
			t.Fatalf("message %d = %+v, want the plain string form", i, c)
		}
	}
	if got := *msgs[0].Content.StringValue; got != "rules" {
		t.Fatalf("system content = %q", got)
	}
	if got := *msgs[3].Content.StringValue; got != "plain" {
		t.Fatalf("text-only user content = %q", got)
	}

	withText := msgs[1].Content
	if withText == nil || withText.StringValue != nil || len(withText.ListValue) != 2 {
		t.Fatalf("text and image = %+v, want a list of two parts", withText)
	}
	if p := withText.ListValue[0]; p.Type != arkmodel.ChatCompletionMessageContentPartTypeText || p.Text != "What is in this image?" || p.ImageURL != nil {
		t.Fatalf("first part = %+v, want the text", p)
	}
	if p := withText.ListValue[1]; p.Type != arkmodel.ChatCompletionMessageContentPartTypeImageURL || p.Text != "" || p.ImageURL == nil ||
		p.ImageURL.URL != cat || p.ImageURL.Detail != "" {
		t.Fatalf("second part = %+v, want the image with no detail", p)
	}

	imageOnly := msgs[2].Content
	if imageOnly == nil || imageOnly.StringValue != nil || len(imageOnly.ListValue) != 1 {
		t.Fatalf("image only = %+v, want a list of one part", imageOnly)
	}
	if p := imageOnly.ListValue[0]; p.Type != arkmodel.ChatCompletionMessageContentPartTypeImageURL || p.ImageURL == nil ||
		p.ImageURL.URL != cat || p.ImageURL.Detail != arkmodel.ImageURLDetailHigh {
		t.Fatalf("image part = %+v, want the image with its detail", p)
	}

	// The wire carries the same thing: no detail key where none was set. Role and content are
	// what the translation decides; the SDK adds its own null fields around them.
	data, err := jsonv2.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"role":"system","content":"rules"},` +
		`{"role":"user","content":[{"type":"text","text":"What is in this image?"},{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png","detail":"high"}}]},` +
		`{"role":"user","content":"plain"}]`
	type wireMessage struct {
		Role    string         `json:"role"`
		Content jsontext.Value `json:"content"`
	}
	var gotWire, wantWire []wireMessage
	if err := jsonv2.Unmarshal(data, &gotWire); err != nil {
		t.Fatal(err)
	}
	if err := jsonv2.Unmarshal([]byte(want), &wantWire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotWire, wantWire) {
		t.Fatalf("wire = %s, want %s", data, want)
	}
}

// ReasoningTokensKnown separates "the provider reported zero" from "the provider did not
// report", which a probe report has to keep apart.
func TestTranslateUsage(t *testing.T) {
	if got := translateUsage(nil, true); got != (loom.Usage{}) {
		t.Fatalf("nil usage = %+v", got)
	}
	usage := &arkmodel.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	usage.CompletionTokensDetails.ReasoningTokens = 4
	if got := translateUsage(usage, true); got.ReasoningTokens != 4 || !got.ReasoningTokensKnown {
		t.Fatalf("usage = %+v", got)
	}
	if got := translateUsage(usage, false); got.ReasoningTokensKnown {
		t.Fatalf("unreported reasoning marked known: %+v", got)
	}
}
