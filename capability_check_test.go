package loom

import (
	"strings"
	"testing"
)

// TestCheckRequestAgainstCapabilities is the capability-check matrix: a request
// that uses a capability declared unsupported fails, and an undeclared capability,
// a zero value, passes through.
func TestCheckRequestAgainstCapabilities(t *testing.T) {
	jsonSchemaReq := ChatRequest{StructuredOutput: &StructuredOutput{Mode: StructuredOutputJSONSchema}}
	jsonObjectReq := ChatRequest{StructuredOutput: &StructuredOutput{Mode: StructuredOutputJSONObject}}
	responseFormatReq := ChatRequest{ResponseFormat: ResponseFormatJSONObject}

	tests := []struct {
		name    string
		caps    ModelCapabilities
		req     ChatRequest
		wantErr string // non-empty = an error whose message contains this substring
	}{
		// ===== a json_schema request =====
		{"schema x declared schema", ModelCapabilities{StructuredOutput: StructuredOutputJSONSchema}, jsonSchemaReq, ""},
		{"schema x declared object", ModelCapabilities{StructuredOutput: StructuredOutputJSONObject}, jsonSchemaReq, "does not support json_schema"},
		{"schema x declared none", ModelCapabilities{StructuredOutput: StructuredOutputNone}, jsonSchemaReq, "does not support json_schema"},
		{"schema x undeclared passes through", ModelCapabilities{}, jsonSchemaReq, ""},

		// ===== a json_object request =====
		{"object x declared object", ModelCapabilities{StructuredOutput: StructuredOutputJSONObject}, jsonObjectReq, ""},
		{"object x declared schema", ModelCapabilities{StructuredOutput: StructuredOutputJSONSchema}, jsonObjectReq, ""},
		{"object x declared none", ModelCapabilities{StructuredOutput: StructuredOutputNone}, jsonObjectReq, "does not support json_object"},
		{"object x undeclared passes through", ModelCapabilities{}, jsonObjectReq, ""},

		// ===== a response_format request =====
		{"rf-object x declared object", ModelCapabilities{StructuredOutput: StructuredOutputJSONObject}, responseFormatReq, ""},
		{"rf-object x declared none", ModelCapabilities{StructuredOutput: StructuredOutputNone}, responseFormatReq, "does not support response_format"},
		{"rf-object x undeclared passes through", ModelCapabilities{}, responseFormatReq, ""},

		// ===== an invalid value on the request side =====
		{"request Mode none is invalid", ModelCapabilities{}, ChatRequest{StructuredOutput: &StructuredOutput{Mode: StructuredOutputNone}}, "reserved for capability declarations"},

		// ===== MaxTokens =====
		{"MaxTokens above the limit", ModelCapabilities{MaxOutputTokens: 100}, ChatRequest{MaxTokens: new(int(200))}, "exceeds the output limit"},
		{"MaxTokens within the limit", ModelCapabilities{MaxOutputTokens: 100}, ChatRequest{MaxTokens: new(int(50))}, ""},
		{"MaxTokens x unknown limit passes through", ModelCapabilities{}, ChatRequest{MaxTokens: new(int(999999))}, ""},

		// ===== no relevant request field =====
		{"plain request x declared none", ModelCapabilities{StructuredOutput: StructuredOutputNone}, ChatRequest{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckRequestAgainstCapabilities(tt.caps, tt.req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got none", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// Image input is the one capability that fails closed. Vision has no "undeclared" state that
// lets a request through: a model that has not declared it refuses an image before anything
// is sent, and a request without images is never affected.
func TestCheckRequestAgainstCapabilitiesVision(t *testing.T) {
	image := ImagePart("https://example.com/cat.png", "")
	user := func(parts ...ContentPart) ChatRequest {
		return ChatRequest{Messages: []Message{{Role: RoleUser, Parts: parts}}}
	}
	tests := []struct {
		name    string
		caps    ModelCapabilities
		req     ChatRequest
		wantErr string // non-empty = an error whose message contains this substring
	}{
		{"image x vision declared", ModelCapabilities{Vision: true}, user(image), ""},
		{"image after text x vision declared", ModelCapabilities{Vision: true}, user(TextPart("what is this?"), image), ""},
		{"image x vision undeclared", ModelCapabilities{}, user(image), "does not declare vision support"},
		{"image x vision declared false", ModelCapabilities{Vision: false}, user(image), "does not declare vision support"},
		{"text x vision undeclared", ModelCapabilities{}, user(TextPart("hello")), ""},
		{"no parts x vision undeclared", ModelCapabilities{}, user(), ""},
		{"image on a later message x undeclared", ModelCapabilities{}, ChatRequest{Messages: []Message{
			{Role: RoleUser, Parts: []ContentPart{TextPart("hello")}},
			{Role: RoleUser, Parts: []ContentPart{image}},
		}}, "message 1 carries an image part"},
		// Vision is asked first: an undeclared model is refused whatever role carries the image.
		{"image on an assistant message x undeclared", ModelCapabilities{}, ChatRequest{Messages: []Message{
			{Role: RoleUser, Parts: []ContentPart{TextPart("hello")}},
			{Role: RoleAssistant, Parts: []ContentPart{image}},
		}}, "does not declare vision support"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckRequestAgainstCapabilities(tt.caps, tt.req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got none", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// An image belongs on a user message. DeepSeek and OpenRouter refuse one on an
// assistant message, and on a system or tool message it could not be put on the wire at all,
// so the request is refused rather than sent without the image.
func TestAssistantMessageImagePartRejected(t *testing.T) {
	parts := []ContentPart{TextPart("look at this"), ImagePart("https://example.com/cat.png", "high")}
	for _, role := range []Role{RoleSystem, RoleAssistant, RoleTool} {
		t.Run(string(role), func(t *testing.T) {
			err := CheckRequestAgainstCapabilities(ModelCapabilities{Vision: true}, ChatRequest{Messages: []Message{
				{Role: RoleUser, Parts: []ContentPart{TextPart("task")}},
				{Role: role, Parts: parts, ToolCallID: "call-1"},
			}})
			if err == nil {
				t.Fatal("expected the image part to be refused")
			}
			if want := `message 1 has role "` + string(role) + `"`; !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name the message and its role, %q", err.Error(), want)
			}
			if !strings.Contains(err.Error(), "only a user message can carry one") {
				t.Fatalf("error %q does not say where an image may go", err.Error())
			}
		})
	}
	// The same parts are a valid user message once the model has declared vision.
	if err := CheckRequestAgainstCapabilities(ModelCapabilities{Vision: true}, ChatRequest{Messages: []Message{
		{Role: RoleUser, Parts: parts},
	}}); err != nil {
		t.Fatalf("an image on a user message: %v", err)
	}
}
