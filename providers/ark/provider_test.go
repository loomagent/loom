package ark

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"

	"github.com/loomagent/loom"
)

func TestRequestHeadersAreSnapshottedAndSent(t *testing.T) {
	const headerName = "X-Ark-Policy"
	const headerValue = "reviewed-policy"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(headerName); got != headerValue {
			t.Errorf("%s = %q, want %q", headerName, got, headerValue)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"response","model":"ep-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer server.Close()

	headers := map[string]string{headerName: headerValue}
	model, err := New(Config{
		APIKey: "test-key", ModelName: "ep-test", BaseURL: server.URL,
		RequestHeaders: headers,
	})
	if err != nil {
		t.Fatal(err)
	}
	headers[headerName] = "mutated-after-construction"
	response, err := model.chatRaw(context.Background(), arkmodel.CreateChatCompletionRequest{Model: "ep-test"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "ok" {
		t.Fatalf("content = %q, want ok", response.Content)
	}
}

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

func TestArkManuallyDeclaredEffortWireAndRejection(t *testing.T) {
	for _, tc := range []struct {
		model   string
		effort  loom.ReasoningEffort
		wantErr bool
	}{
		{"doubao-seed-evolving", "medium", false},
		{"doubao-seed-evolving", "max", true},
		{"deepseek-v4-pro-ga-260813", "max", false},
		{"deepseek-v4-pro-ga-260813", "medium", true},
	} {
		efforts := []loom.ReasoningEffort{"low", "high", "max"}
		if tc.model == "doubao-seed-evolving" {
			efforts = []loom.ReasoningEffort{"low", "medium", "high", "minimal"}
		}
		caps := loom.ModelCapabilities{Reasoning: loom.ReasoningSupportToggleable, ReasoningEfforts: efforts}
		m, err := New(Config{APIKey: "test", ModelName: tc.model, Capabilities: &caps})
		if err != nil {
			t.Fatal(err)
		}
		body, err := m.ReasoningRequestParameters(loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: tc.effort})
		if tc.wantErr {
			var local *loom.RequestValidationError
			if !errors.As(err, &local) {
				t.Fatalf("expected local alias error: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if body["reasoning_effort"] != string(tc.effort) || body["thinking"].(map[string]any)["type"] != "enabled" {
			t.Fatalf("body=%+v", body)
		}
		body, err = m.ReasoningRequestParameters(loom.Reasoning{Mode: loom.ReasoningModeDisabled})
		if err != nil {
			t.Fatal(err)
		}
		if body["reasoning_effort"] != nil || body["thinking"].(map[string]any)["type"] != "disabled" {
			t.Fatalf("body=%+v", body)
		}
	}
}

func TestExplicitNoneDeclarationRejectsEnabledBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	caps := loom.ModelCapabilities{Reasoning: loom.ReasoningSupportNone}
	model, err := New(Config{APIKey: "test", ModelName: "doubao-seed-evolving", BaseURL: server.URL, Capabilities: &caps})
	if err != nil {
		t.Fatal(err)
	}
	_, err = model.Chat(context.Background(), loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "hello"}}, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled}})
	var local *loom.RequestValidationError
	if !errors.As(err, &local) {
		t.Fatalf("expected local contradiction, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid declaration sent %d HTTP requests", calls.Load())
	}
	probeCaps := loom.ReasoningProbeCapabilities(true)
	model, err = New(Config{APIKey: "test", ModelName: "doubao-seed-evolving", Capabilities: &probeCaps})
	if err != nil {
		t.Fatal(err)
	}
	params, err := model.ReasoningRequestParameters(loom.Reasoning{Mode: loom.ReasoningModeDisabled})
	if err != nil || len(params) != 0 {
		t.Fatalf("isolated default probe must omit: params=%+v err=%v", params, err)
	}
}
