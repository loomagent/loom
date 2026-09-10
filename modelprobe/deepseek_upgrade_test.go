package modelprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/deepseek"
	goseek "github.com/storynap/goseek"
)

func TestDeepSeekSchemaUpgradeBeyondDeclaredCapabilities(t *testing.T) {
	for _, behavior := range []string{"reject", "support", "ignore", "unavailable"} {
		t.Run(behavior, func(t *testing.T) {
			var schemaCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Thinking *struct {
						Type string `json:"type"`
					} `json:"thinking"`
					ResponseFormat *struct {
						Type       string          `json:"type"`
						JSONSchema json.RawMessage `json:"json_schema"`
					} `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				content := `{"ok":true}`
				if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_schema" {
					schemaCalls.Add(1)
					if len(req.ResponseFormat.JSONSchema) == 0 {
						t.Error("schema missing on wire")
					}
					switch behavior {
					case "reject":
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":{"message":"This response_format type is unavailable now"}}`)
						return
					case "unavailable":
						w.WriteHeader(503)
						fmt.Fprint(w, `{"error":{"message":"temporarily unavailable"}}`)
						return
					case "ignore":
						content = `{"wrong":"schema ignored"}`
					}
				}
				reasoning, tokens := "baseline", 3
				if req.Thinking != nil && req.Thinking.Type == "disabled" {
					reasoning, tokens = "", 0
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"model":   "deepseek-flash",
					"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content, "reasoning_content": reasoning}}},
					"usage":   map[string]any{"completion_tokens_details": map[string]any{"reasoning_tokens": tokens}},
				})
			}))
			defer server.Close()
			builder := BuilderFunc(func(_ context.Context, caps loom.ModelCapabilities) (loom.ChatModel, error) {
				return deepseek.New(deepseek.Config{APIKey: "test", BaseURL: server.URL, ModelName: "deepseek-flash", Capabilities: &caps, Retry: &loom.RetryConfig{Mode: loom.RetryModeDisabled}})
			})
			declared := loom.ModelCapabilities{StructuredOutput: loom.StructuredOutputJSONObject, Reasoning: loom.ReasoningSupportToggleable, ReasoningEfforts: []loom.ReasoningEffort{loom.ReasoningEffortHigh}}
			report, err := Probe(t.Context(), builder, Options{
				DeclaredCapabilities: &declared,
				ErrorClassifier: func(err error) ErrorDisposition {
					var apiErr *goseek.APIError
					if errors.As(err, &apiErr) && apiErr.StatusCode == 400 && strings.Contains(apiErr.Message, "response_format") {
						return ErrorUnsupported
					}
					return ErrorInconclusive
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if schemaCalls.Load() != 1 {
				t.Fatalf("schema probe calls=%d", schemaCalls.Load())
			}
			want, wantOutcome := loom.StructuredOutputJSONObject, OutcomeNegative
			switch behavior {
			case "support":
				want, wantOutcome = loom.StructuredOutputJSONSchema, OutcomePositive
			case "unavailable":
				want, wantOutcome = loom.StructuredOutputUnsupported, OutcomeError
			}
			if report.Observed.StructuredOutput != want || report.Coverage.StructuredOutput != (behavior != "unavailable") {
				t.Fatalf("observed=%+v coverage=%+v", report.Observed, report.Coverage)
			}
			foundDiff := false
			for _, d := range Compare(declared, report) {
				if d.Field == "structured_output" {
					foundDiff = d.Declared == "json_object" && d.Observed == "json_schema"
				}
			}
			if foundDiff != (behavior == "support") {
				t.Fatalf("upgrade diff=%+v", Compare(declared, report))
			}
			for _, check := range report.Checks {
				if check.Name == CheckStructuredJSONSchema && check.Outcome != wantOutcome {
					t.Fatalf("schema check=%+v", check)
				}
				if check.Name == CheckReasoningDisable && check.Outcome != OutcomePositive {
					t.Fatalf("explicit zero telemetry lost: %+v", check)
				}
			}
		})
	}
}
