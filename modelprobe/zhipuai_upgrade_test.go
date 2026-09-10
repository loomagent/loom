package modelprobe

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/zhipuai"
)

func TestZhipuSchemaUpgradeBeyondDeclaredCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		code, message string
		outcome       Outcome
	}{
		{name: "support", outcome: OutcomePositive},
		{name: "ignore", outcome: OutcomeNegative},
		{name: "truncated", outcome: OutcomeError},
		{name: "abnormal finish", outcome: OutcomeError},
		{name: "reject", status: 400, code: "1210", message: "response_format json_schema is unsupported", outcome: OutcomeNegative},
		{name: "unavailable format", status: 400, code: "1210", message: "This response_format type is unavailable now", outcome: OutcomeNegative},
		{name: "invalid schema", status: 400, code: "1210", message: "response_format.json_schema.schema is invalid: missing required property", outcome: OutcomeError},
		{name: "unsupported schema keyword", status: 400, code: "1210", message: "response_format.json_schema.schema: unsupported keyword const", outcome: OutcomeError},
		{name: "unsupported schema constraint", status: 400, code: "1210", message: "response_format: unsupported schema constraint minimum", outcome: OutcomeError},
		{name: "ordinary parameter", status: 400, code: "1210", message: "temperature is unsupported", outcome: OutcomeError},
		{name: "model missing", status: 400, code: "1211", message: "model does not exist", outcome: OutcomeError},
		{name: "unauthorized", status: 401, code: "1000", message: "authentication failed", outcome: OutcomeError},
		{name: "forbidden", status: 403, code: "1003", message: "permission denied", outcome: OutcomeError},
		{name: "balance", status: 429, code: "1113", message: "insufficient balance", outcome: OutcomeError},
		{name: "rate limit", status: 429, code: "1302", message: "rate limited", outcome: OutcomeError},
		{name: "service unavailable", status: 503, code: "1200", message: "unavailable", outcome: OutcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var schemaCalls, objectCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model    string `json:"model"`
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
					ResponseFormat *struct {
						Type       string `json:"type"`
						JSONSchema struct {
							Name   string             `json:"name"`
							Strict bool               `json:"strict"`
							Schema *jsonschema.Schema `json:"schema"`
						} `json:"json_schema"`
					} `json:"response_format"`
				}
				if err := json.UnmarshalRead(r.Body, &req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if r.URL.Path != "/chat/completions" || req.Model != "glm-5.3" {
					t.Errorf("path=%s model=%s", r.URL.Path, req.Model)
				}
				content, finish := `{"ok":true}`, "stop"
				if req.ResponseFormat != nil {
					switch req.ResponseFormat.Type {
					case "json_object":
						objectCalls.Add(1)
					case "json_schema":
						schemaCalls.Add(1)
						format := req.ResponseFormat.JSONSchema
						if format.Schema == nil || !format.Strict || format.Name != "loom_model_probe" {
							t.Error("incomplete schema on wire")
							w.WriteHeader(400)
							return
						}
						nonce, ok := format.Schema.Properties["nonce"]
						if !ok || nonce.Const == nil {
							t.Error("missing randomized constraint")
							w.WriteHeader(400)
							return
						}
						for _, message := range req.Messages {
							if strings.Contains(message.Content, fmt.Sprint(*nonce.Const)) {
								t.Error("constraint copied to prompt")
							}
						}
						if tc.status != 0 {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(tc.status)
							fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`, tc.code, tc.message)
							return
						}
						if tc.name == "support" {
							data, err := json.Marshal(map[string]any{"ok": true, "nonce": *nonce.Const})
							if err != nil {
								t.Error(err)
								return
							}
							content = string(data)
						}
						if tc.name == "truncated" {
							content, finish = `{`, "length"
						}
						if tc.name == "abnormal finish" {
							finish = "network_error"
						}
					default:
						t.Errorf("unexpected response_format=%s", req.ResponseFormat.Type)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"model":"glm-response-version","choices":[{"index":0,"finish_reason":%q,"message":{"role":"assistant","content":%q,"reasoning_content":"observed reasoning"}}],"usage":{"completion_tokens_details":{"reasoning_tokens":4}}}`, finish, content)
			}))
			defer server.Close()
			declared := loom.ModelCapabilities{StructuredOutput: loom.StructuredOutputJSONObject, Reasoning: loom.ReasoningSupportAlwaysOn, ReasoningEfforts: []loom.ReasoningEffort{"low"}}
			builder := BuilderFunc(func(_ context.Context, caps loom.ModelCapabilities) (loom.ChatModel, error) {
				if caps.StructuredOutput != "" {
					t.Fatalf("business structured gate leaked into probe: %+v", caps)
				}
				return zhipuai.New(zhipuai.Config{APIKey: "test", ModelName: "glm-5.3", BaseURL: server.URL, Capabilities: &caps, Retry: &loom.RetryConfig{Mode: loom.RetryModeDisabled}})
			})
			report, err := Probe(t.Context(), builder, Options{DeclaredCapabilities: &declared})
			if err != nil {
				t.Fatal(err)
			}
			if declared.StructuredOutput != loom.StructuredOutputJSONObject || objectCalls.Load() != 1 || schemaCalls.Load() != 1 {
				t.Fatalf("declaration=%+v object=%d schema=%d", declared, objectCalls.Load(), schemaCalls.Load())
			}
			object, schema := report.Checks[len(report.Checks)-2], report.Checks[len(report.Checks)-1]
			if object.Outcome != OutcomePositive || schema.Outcome != tc.outcome {
				t.Fatalf("object=%+v schema=%+v", object, schema)
			}
			want := loom.StructuredOutputJSONObject
			if tc.outcome == OutcomePositive {
				want = loom.StructuredOutputJSONSchema
			}
			if report.Observed.StructuredOutput != want || report.Coverage.StructuredOutput != (tc.outcome != OutcomeError) {
				t.Fatalf("report=%+v", report)
			}
			if tc.status != 0 && schema.Evidence.Acceptance == "local_rejected" {
				t.Fatalf("server rejection reported as local: %+v", schema)
			}
			if tc.name == "support" && schema.Evidence.ResponseModel != "glm-response-version" {
				t.Fatalf("response model lost: %+v", schema)
			}
			upgrade := false
			for _, mismatch := range Compare(declared, report) {
				if mismatch.Field == "structured_output" {
					upgrade = mismatch.Declared == "json_object" && mismatch.Observed == "json_schema"
				}
			}
			if upgrade != (tc.name == "support") {
				t.Fatalf("unexpected capability comparison: %+v", Compare(declared, report))
			}
		})
	}
}

func TestZhipuUpstreamFailurePreservesPriorStructuredEvidence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		objectContent string
		objectStatus  int
		objectOutcome Outcome
		observed      loom.StructuredOutputMode
	}{
		{"prior success", `{"ok":true}`, 200, OutcomePositive, loom.StructuredOutputJSONObject},
		{"prior confirmed failure", `[]`, 200, OutcomeNegative, loom.StructuredOutputUnsupported},
		{"both unavailable", "", 503, OutcomeError, loom.StructuredOutputUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var objectCalls, schemaCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ResponseFormat struct {
						Type string `json:"type"`
					} `json:"response_format"`
				}
				if err := json.UnmarshalRead(r.Body, &req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				content, status := "reasoned answer", 200
				switch req.ResponseFormat.Type {
				case "json_object":
					objectCalls.Add(1)
					content, status = tc.objectContent, tc.objectStatus
				case "json_schema":
					schemaCalls.Add(1)
					status = 503
				case "":
				default:
					t.Errorf("unexpected response format %s", req.ResponseFormat.Type)
					w.WriteHeader(400)
					return
				}
				if status == 503 {
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":{"code":"1200","message":"temporarily unavailable"}}`)
					return
				}
				fmt.Fprintf(w, `{"model":"glm-response-version","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q,"reasoning_content":"observed reasoning"}}]}`, content)
			}))
			defer server.Close()
			builder := BuilderFunc(func(_ context.Context, caps loom.ModelCapabilities) (loom.ChatModel, error) {
				return zhipuai.New(zhipuai.Config{APIKey: "test", ModelName: "glm-5.3", BaseURL: server.URL, Capabilities: &caps, Retry: &loom.RetryConfig{Mode: loom.RetryModeDisabled}})
			})
			declared := loom.ModelCapabilities{StructuredOutput: loom.StructuredOutputJSONSchema, Reasoning: loom.ReasoningSupportAlwaysOn}
			// Upstream errors belong to their checks. Probe can finish the audit
			// with inconclusive checks without returning a context/run error.
			report, err := Probe(t.Context(), builder, Options{DeclaredCapabilities: &declared, ErrorClassifier: func(error) ErrorDisposition { return ErrorUnsupported }})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Checks) != 5 || objectCalls.Load() != 1 || schemaCalls.Load() != 1 {
				t.Fatalf("report=%+v object=%d schema=%d", report, objectCalls.Load(), schemaCalls.Load())
			}
			// Serialization must preserve earlier positive/negative evidence as
			// well as the later HTTP failure, with no fabricated complete profile.
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var stored Report
			if err := json.Unmarshal(data, &stored); err != nil {
				t.Fatal(err)
			}
			object, schema := stored.Checks[3], stored.Checks[4]
			if object.Outcome != tc.objectOutcome || schema.Outcome != OutcomeError || schema.Evidence.Acceptance != "unknown" || !strings.Contains(schema.Evidence.Error, "HTTP 503") {
				t.Fatalf("object=%+v schema=%+v", object, schema)
			}
			if stored.Observed.StructuredOutput != tc.observed || stored.Coverage.StructuredOutput || stored.Observed.StructuredOutput == loom.StructuredOutputNone {
				t.Fatalf("unavailable checks inferred a capability profile: %+v", stored)
			}
			if len(Compare(declared, stored)) != 0 {
				t.Fatalf("incomplete profile generated downgrade: %+v", Compare(declared, stored))
			}
			if tc.objectStatus == 200 && (object.Evidence.Acceptance != "accepted" || object.Evidence.ResponsePreview != tc.objectContent || object.Evidence.FinishReason != loom.FinishReasonStop) {
				t.Fatalf("prior evidence lost: %+v", object)
			}
		})
	}
}
