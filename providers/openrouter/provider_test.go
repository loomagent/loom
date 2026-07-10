package openrouter

import (
	"strings"
	"testing"

	"github.com/loomagent/loom"
)

// TestBuildRequestReasoningModeRequired 必传契约的 provider 级兜底:
// 调用方忘传 Reasoning.Mode 时,请求在构造阶段就报错,绝不发出去。
func TestBuildRequestReasoningModeRequired(t *testing.T) {
	m, err := New(Config{APIKey: "test-key", ModelName: "x-ai/grok-4.3"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.buildRequest(loom.ChatRequest{
		Messages: []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
		// 故意不传 Reasoning
	})
	if err == nil {
		t.Fatal("期望 Reasoning.Mode 未传报错,实际成功")
	}
	if !strings.Contains(err.Error(), "必传") {
		t.Fatalf("错误信息 %q 不含 \"必传\"", err.Error())
	}
}

// TestBuildRequestReasoningModeExplicit 显式传 enabled/disabled 映射到
// OpenRouter 统一 reasoning 对象(经请求 ExtraFields 注入)。
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
		extra := req.GetExtraFields()
		reasoning, ok := extra["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("mode=%s 期望 ExtraFields 带 reasoning 对象,实际 %v", tt.mode, extra)
		}
		if reasoning["enabled"] != tt.wantEnabled {
			t.Fatalf("mode=%s reasoning.enabled = %v, want %v", tt.mode, reasoning["enabled"], tt.wantEnabled)
		}
		if tt.wantEffort != "" && reasoning["effort"] != tt.wantEffort {
			t.Fatalf("mode=%s reasoning.effort = %v, want %v", tt.mode, reasoning["effort"], tt.wantEffort)
		}
	}
}

// TestBuildRequestReasoningEffortMaxRejected max 是 deepseek 专属档位,openrouter 报错。
func TestBuildRequestReasoningEffortMaxRejected(t *testing.T) {
	m, err := New(Config{APIKey: "test-key", ModelName: "x-ai/grok-4.3"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.buildRequest(loom.ChatRequest{
		Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
		Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: loom.ReasoningEffortMax},
	})
	if err == nil || !strings.Contains(err.Error(), "不支持 reasoning effort") {
		t.Fatalf("期望 max 档位报错,实际: %v", err)
	}
}
