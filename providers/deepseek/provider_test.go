package deepseek

import (
	"errors"
	"strings"
	"testing"

	goseek "github.com/storynap/goseek"

	"github.com/loomagent/loom"
)

// TestBuildRequestReasoningModeRequired 必传契约的 provider 级兜底:
// 调用方忘传 Reasoning.Mode 时,请求在构造阶段就报错,绝不发出去。
func TestBuildRequestReasoningModeRequired(t *testing.T) {
	m, err := New(Config{APIKey: "test-key"})
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

// TestBuildRequestReasoningModeExplicit 显式传 enabled/disabled 正常构造。
func TestBuildRequestReasoningModeExplicit(t *testing.T) {
	m, err := New(Config{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, mode := range []loom.ReasoningMode{loom.ReasoningModeEnabled, loom.ReasoningModeDisabled} {
		req, err := m.buildRequest(loom.ChatRequest{
			Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
			Reasoning: loom.Reasoning{Mode: mode},
		})
		if err != nil {
			t.Fatalf("mode=%s buildRequest: %v", mode, err)
		}
		if req.Thinking == nil {
			t.Fatalf("mode=%s 期望显式发送 thinking 字段,实际为 nil", mode)
		}
		if string(req.Thinking.Type) != string(mode) {
			t.Fatalf("mode=%s thinking.type = %q", mode, req.Thinking.Type)
		}
	}
}

func TestNormalizeDeepSeekContentExistsRisk(t *testing.T) {
	apiErr := &goseek.APIError{
		StatusCode: 400,
		Message:    "Content Exists Risk",
	}
	err := normalizeDeepSeekError(apiErr)

	if !errors.Is(err, loom.ErrSensitiveContentRisk) {
		t.Fatalf("normalized error = %v, want ErrSensitiveContentRisk", err)
	}
	var got *goseek.APIError
	if !errors.As(err, &got) || got != apiErr {
		t.Fatalf("normalized error does not preserve original APIError")
	}
	if (classifier{}).ClassifyError(err) != loom.ErrorClassPermanent {
		t.Fatalf("classifier = %v, want permanent", (classifier{}).ClassifyError(err))
	}
}

func TestNormalizeDeepSeekContentExistsRiskRequiresOfficial400(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "same message different status",
			err:  &goseek.APIError{StatusCode: 500, Message: "Content Exists Risk"},
		},
		{
			name: "ordinary bad request",
			err:  &goseek.APIError{StatusCode: 400, Message: "invalid request"},
		},
		{
			name: "plain string is not enough",
			err:  errors.New("Content Exists Risk"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := normalizeDeepSeekError(tt.err)
			if errors.Is(err, loom.ErrSensitiveContentRisk) {
				t.Fatalf("normalized error = %v, did not expect ErrSensitiveContentRisk", err)
			}
		})
	}
}
