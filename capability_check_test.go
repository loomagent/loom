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
