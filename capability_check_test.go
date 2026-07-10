package loom

import (
	"strings"
	"testing"
)

// TestCheckRequestAgainstCapabilities 防御性能力校验矩阵:
// 请求用到声明不支持的能力 → 报错;能力未声明(零值)→ 透传。
func TestCheckRequestAgainstCapabilities(t *testing.T) {
	jsonSchemaReq := ChatRequest{StructuredOutput: &StructuredOutput{Mode: StructuredOutputJSONSchema}}
	jsonObjectReq := ChatRequest{StructuredOutput: &StructuredOutput{Mode: StructuredOutputJSONObject}}
	responseFormatReq := ChatRequest{ResponseFormat: ResponseFormatJSONObject}

	tests := []struct {
		name    string
		caps    ModelCapabilities
		req     ChatRequest
		wantErr string // 非空 = 期望报错且错误信息包含该子串
	}{
		// ===== json_schema 请求 =====
		{"schema×声明schema", ModelCapabilities{StructuredOutput: StructuredOutputJSONSchema}, jsonSchemaReq, ""},
		{"schema×声明object", ModelCapabilities{StructuredOutput: StructuredOutputJSONObject}, jsonSchemaReq, "不支持 json_schema"},
		{"schema×声明none", ModelCapabilities{StructuredOutput: StructuredOutputNone}, jsonSchemaReq, "不支持 json_schema"},
		{"schema×未声明透传", ModelCapabilities{}, jsonSchemaReq, ""},

		// ===== json_object 请求 =====
		{"object×声明object", ModelCapabilities{StructuredOutput: StructuredOutputJSONObject}, jsonObjectReq, ""},
		{"object×声明schema", ModelCapabilities{StructuredOutput: StructuredOutputJSONSchema}, jsonObjectReq, ""},
		{"object×声明none", ModelCapabilities{StructuredOutput: StructuredOutputNone}, jsonObjectReq, "不支持 json_object"},
		{"object×未声明透传", ModelCapabilities{}, jsonObjectReq, ""},

		// ===== response_format 请求 =====
		{"rf-object×声明object", ModelCapabilities{StructuredOutput: StructuredOutputJSONObject}, responseFormatReq, ""},
		{"rf-object×声明none", ModelCapabilities{StructuredOutput: StructuredOutputNone}, responseFormatReq, "不支持 response_format"},
		{"rf-object×未声明透传", ModelCapabilities{}, responseFormatReq, ""},

		// ===== 请求侧非法值 =====
		{"请求Mode取none非法", ModelCapabilities{}, ChatRequest{StructuredOutput: &StructuredOutput{Mode: StructuredOutputNone}}, "能力声明专用值"},

		// ===== MaxTokens =====
		{"MaxTokens超上限", ModelCapabilities{MaxOutputTokens: 100}, ChatRequest{MaxTokens: new(int(200))}, "超过模型声明的输出上限"},
		{"MaxTokens在上限内", ModelCapabilities{MaxOutputTokens: 100}, ChatRequest{MaxTokens: new(int(50))}, ""},
		{"MaxTokens×上限未知透传", ModelCapabilities{}, ChatRequest{MaxTokens: new(int(999999))}, ""},

		// ===== 无相关请求字段 =====
		{"普通请求×声明none", ModelCapabilities{StructuredOutput: StructuredOutputNone}, ChatRequest{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckRequestAgainstCapabilities(tt.caps, tt.req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过,实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望报错(含 %q),实际通过", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("错误信息 %q 不含 %q", err.Error(), tt.wantErr)
			}
		})
	}
}
