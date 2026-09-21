package openrouter

import (
	json "encoding/json/v2"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/loomagent/loom"
)

func TestClassifierTreats503AsTransient(t *testing.T) {
	err := &openai.Error{StatusCode: 503}
	if got := (classifier{}).ClassifyError(err); got != loom.ErrorClassTransient {
		t.Fatalf("503 class = %s, want transient", got)
	}
	if !(classifier{}).IsServiceUnavailable(err) {
		t.Fatal("503 must open the shared service-unavailable circuit")
	}
}

// TestBuildRequestReasoningModeRequired is the provider-level backstop for the
// required-Mode contract: a caller that forgets Reasoning.Mode fails while the
// request is being built, and nothing is sent.
func TestBuildRequestReasoningModeRequired(t *testing.T) {
	m, err := New(Config{APIKey: "test-key", ModelName: "x-ai/grok-4.3"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.buildRequest(loom.ChatRequest{
		Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
		// Reasoning is deliberately omitted
	})
	if err == nil {
		t.Fatal("a missing Reasoning.Mode must fail")
	}
	if !strings.Contains(err.Error(), "is required") {
		t.Fatalf("error %q does not say the mode is required", err.Error())
	}
}

func TestBuildRequestRejectsUnknownMessageRole(t *testing.T) {
	m, err := New(Config{APIKey: "test-key", ModelName: "x-ai/grok-4.3"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.buildRequest(loom.ChatRequest{
		Messages:  []loom.Message{{Role: loom.Role("invalid"), Content: "hi"}},
		Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
	})
	if err == nil || !strings.Contains(err.Error(), `unknown role "invalid"`) {
		t.Fatalf("expected an unknown-role error, got %v", err)
	}
}

// TestBuildRequestReasoningModeExplicit checks that an explicit enabled or disabled maps
// onto OpenRouter's single reasoning object, injected through the request ExtraFields.
func TestBuildRequestReasoningModeExplicit(t *testing.T) {
	m, err := New(Config{APIKey: "test-key", ModelName: "x-ai/grok-4.3"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		mode        loom.ReasoningMode
		effort      loom.ReasoningEffort
		wantEnabled bool
		wantEffort  string
	}{
		{mode: loom.ReasoningModeEnabled, wantEnabled: true},
		{mode: loom.ReasoningModeEnabled, effort: loom.ReasoningEffortHigh, wantEnabled: true, wantEffort: "high"},
		{mode: loom.ReasoningModeDisabled, wantEnabled: false},
	}
	for _, tt := range tests {
		req, err := m.buildRequest(loom.ChatRequest{
			Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
			Reasoning: loom.Reasoning{Mode: tt.mode, Effort: tt.effort},
		})
		if err != nil {
			t.Fatalf("mode=%s buildRequest: %v", tt.mode, err)
		}
		encoded, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("mode=%s marshal request: %v", tt.mode, err)
		}
		var body map[string]any
		if err := json.Unmarshal(encoded, &body); err != nil {
			t.Fatalf("mode=%s unmarshal request: %v", tt.mode, err)
		}
		reasoning, ok := body["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("mode=%s expected a reasoning object in the request, got %s", tt.mode, encoded)
		}
		if reasoning["enabled"] != tt.wantEnabled {
			t.Fatalf("mode=%s reasoning.enabled = %v, want %v", tt.mode, reasoning["enabled"], tt.wantEnabled)
		}
		if tt.wantEffort != "" && reasoning["effort"] != tt.wantEffort {
			t.Fatalf("mode=%s reasoning.effort = %v, want %v", tt.mode, reasoning["effort"], tt.wantEffort)
		}
	}
}

func TestNativeGatewayEffortsAndDisabled(t *testing.T) {
	for _, effort := range []loom.ReasoningEffort{"minimal", "xhigh", "max"} {
		caps := loom.ModelCapabilities{Reasoning: loom.ReasoningSupportToggleable, ReasoningEfforts: []loom.ReasoningEffort{effort}}
		m, err := New(Config{APIKey: "test", ModelName: "declared-model", Capabilities: &caps})
		if err != nil {
			t.Fatal(err)
		}
		body, err := m.ReasoningRequestParameters(loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: effort})
		if err != nil {
			t.Fatal(err)
		}
		r := body["reasoning"].(map[string]any)
		if r["enabled"] != true || r["effort"] != string(effort) {
			t.Fatalf("changed effort semantics: %+v", body)
		}
		body, err = m.ReasoningRequestParameters(loom.Reasoning{Mode: loom.ReasoningModeDisabled})
		if err != nil {
			t.Fatal(err)
		}
		r = body["reasoning"].(map[string]any)
		if r["enabled"] != false || r["effort"] != nil {
			t.Fatalf("disable: %+v", body)
		}
		if _, err := m.buildRequest(loom.ChatRequest{Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: "unknown"}}); err == nil {
			t.Fatal("undeclared effort accepted")
		}
	}
}
