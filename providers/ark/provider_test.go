package ark

import (
	"strings"
	"testing"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"

	"github.com/loomagent/loom"
)

// TestBuildRequestReasoningModeRequired 必传契约的 provider 级兜底:
// 调用方忘传 Reasoning.Mode 时,请求在构造阶段就报错,绝不发出去。
func TestBuildRequestReasoningModeRequired(t *testing.T) {
	m, err := New(Config{APIKey: "test-key", ModelName: "ep-test"})
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

// TestBuildRequestReasoningModeExplicit 显式传 enabled/disabled 映射到请求级 Thinking 字段。
func TestBuildRequestReasoningModeExplicit(t *testing.T) {
	m, err := New(Config{APIKey: "test-key", ModelName: "ep-test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		mode loom.ReasoningMode
		want arkmodel.ThinkingType
	}{
		{loom.ReasoningModeEnabled, arkmodel.ThinkingTypeEnabled},
		{loom.ReasoningModeDisabled, arkmodel.ThinkingTypeDisabled},
	}
	for _, tt := range tests {
		req, err := m.buildRequest(loom.ChatRequest{
			Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
			Reasoning: loom.Reasoning{Mode: tt.mode},
		})
		if err != nil {
			t.Fatalf("mode=%s buildRequest: %v", tt.mode, err)
		}
		if req.Thinking == nil {
			t.Fatalf("mode=%s 期望显式发送 Thinking 字段,实际为 nil", tt.mode)
		}
		if req.Thinking.Type != tt.want {
			t.Fatalf("mode=%s Thinking.Type = %q, want %q", tt.mode, req.Thinking.Type, tt.want)
		}
	}
}
