package modelprobe

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/loomagent/loom"
)

func validStructuredResponse(t *testing.T, req loom.ChatRequest) string {
	t.Helper()
	if req.StructuredOutput == nil {
		return `{"ok":true}`
	}
	data, err := json.Marshal(map[string]any{"ok": true, "nonce": *req.StructuredOutput.Schema.Properties["nonce"].Const})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStructuredProbeSchemaOnlyRandomConstraints(t *testing.T) {
	var nonces []string
	model := &fakeModel{handler: func(_ loom.ModelCapabilities, req loom.ChatRequest) (*loom.ChatResponse, error) {
		if req.StructuredOutput != nil {
			nonce := (*req.StructuredOutput.Schema.Properties["nonce"].Const).(string)
			if len(nonce) < 16 {
				t.Fatalf("weak nonce=%q", nonce)
			}
			for _, message := range req.Messages {
				if strings.Contains(message.Content, nonce) || strings.Contains(message.Content, `"ok"`) {
					t.Fatalf("constraint leaked to prompt: %q", message.Content)
				}
			}
			nonces = append(nonces, nonce)
		}
		return &loom.ChatResponse{Content: validStructuredResponse(t, req), FinishReason: loom.FinishReasonStop, Model: "response-version"}, nil
	}}
	for range 2 {
		object, schema, err := probeStructuredOutput(t.Context(), model, defaultPerCallTimeout, loom.Reasoning{Mode: loom.ReasoningModeDisabled}, nil)
		if err != nil || object.Outcome != OutcomePositive || schema.Outcome != OutcomePositive {
			t.Fatalf("object=%+v schema=%+v error=%v", object, schema, err)
		}
		data, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		var stored Check
		if err := json.Unmarshal(data, &stored); err != nil {
			t.Fatal(err)
		}
		if stored.Evidence.RequestedResponseFormat != "json_schema" || stored.Evidence.ResponseModel != "response-version" || stored.Evidence.RequestedSchema == nil {
			t.Fatalf("evidence lost: %s", data)
		}
		resolved, err := stored.Evidence.RequestedSchema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal([]byte(stored.Evidence.ResponsePreview), &value); err != nil {
			t.Fatal(err)
		}
		if err := resolved.Validate(value); err != nil {
			t.Fatalf("stored constraints cannot validate evidence: %v", err)
		}
	}
	if len(nonces) != 2 || nonces[0] == nonces[1] {
		t.Fatalf("constraints reused: %v", nonces)
	}
}

func TestStructuredProbeValidatesOnlyNormallyFinishedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, content  string
		finish         loom.FinishReason
		object, schema Outcome
	}{
		{"fixed historical answer", `{"ok":true}`, loom.FinishReasonStop, OutcomePositive, OutcomeNegative},
		{"array", `[]`, loom.FinishReasonStop, OutcomeNegative, OutcomeNegative},
		{"scalar", `true`, loom.FinishReasonStop, OutcomeNegative, OutcomeNegative},
		{"null", `null`, loom.FinishReasonStop, OutcomeNegative, OutcomeNegative},
		{"invalid JSON", `{`, loom.FinishReasonStop, OutcomeNegative, OutcomeNegative},
		{"markdown", "```json\n{\"ok\":true}\n```", loom.FinishReasonStop, OutcomeNegative, OutcomeNegative},
		{"truncated JSON", `{`, loom.FinishReasonLength, OutcomeError, OutcomeError},
		{"length with valid JSON", `{"ok":true}`, loom.FinishReasonLength, OutcomeError, OutcomeError},
		{"missing finish", `{"ok":true}`, "", OutcomeError, OutcomeError},
		{"content filter", `{"ok":true}`, loom.FinishReasonContentFilter, OutcomeError, OutcomeError},
		{"tool calls", `{"ok":true}`, loom.FinishReasonToolCalls, OutcomeError, OutcomeError},
		{"unknown finish", `{"ok":true}`, "network_error", OutcomeError, OutcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &fakeModel{handler: func(loom.ModelCapabilities, loom.ChatRequest) (*loom.ChatResponse, error) {
				return &loom.ChatResponse{Content: tc.content, FinishReason: tc.finish}, nil
			}}
			object, schema, err := probeStructuredOutput(t.Context(), m, defaultPerCallTimeout, loom.Reasoning{Mode: loom.ReasoningModeDisabled}, nil)
			if err != nil || object.Outcome != tc.object || schema.Outcome != tc.schema {
				t.Fatalf("object=%+v schema=%+v error=%v", object, schema, err)
			}
			if schema.Evidence.Acceptance != "accepted" || schema.Evidence.FinishReason != tc.finish {
				t.Fatalf("response evidence lost: %+v", schema)
			}
		})
	}
}

func TestStructuredProbeRetainsIndependentEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		objectError, schemaError error
		want                     loom.StructuredOutputMode
	}{
		{"schema local block", nil, loom.LocalRequestError(loom.ErrUnsupportedCapability), loom.StructuredOutputJSONObject},
		{"schema timeout", nil, context.DeadlineExceeded, loom.StructuredOutputJSONObject},
		{"object unavailable", errors.New("unavailable"), nil, loom.StructuredOutputJSONSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBuilder{handler: func(_ loom.ModelCapabilities, req loom.ChatRequest) (*loom.ChatResponse, error) {
				if req.StructuredOutput != nil && tc.schemaError != nil {
					return nil, tc.schemaError
				}
				if req.ResponseFormat != "" && tc.objectError != nil {
					return nil, tc.objectError
				}
				if req.StructuredOutput != nil || req.ResponseFormat != "" {
					return &loom.ChatResponse{Content: validStructuredResponse(t, req), FinishReason: loom.FinishReasonStop}, nil
				}
				return reasoningResponse(1), nil
			}}
			r, err := Probe(t.Context(), b, Options{ErrorClassifier: func(err error) ErrorDisposition {
				var local *loom.RequestValidationError
				if errors.As(err, &local) {
					return ErrorUnsupported
				}
				return ErrorInconclusive
			}})
			if err != nil || r.Observed.StructuredOutput != tc.want || r.Coverage.StructuredOutput {
				t.Fatalf("report=%+v error=%v", r, err)
			}
			object, schema := r.Checks[3], r.Checks[4]
			if tc.objectError == nil && object.Outcome != OutcomePositive {
				t.Fatalf("Object evidence lost: %+v", object)
			}
			if tc.schemaError != nil && schema.Outcome != OutcomeError {
				t.Fatalf("Schema error misclassified: %+v", schema)
			}
			if tc.schemaError == nil && schema.Outcome != OutcomePositive {
				t.Fatalf("Schema evidence lost: %+v", schema)
			}
		})
	}
}

func TestProbeCancellationPreservesCompletedChecks(t *testing.T) {
	for _, phase := range []string{"default", "object", "schema"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			b := &fakeBuilder{handler: func(_ loom.ModelCapabilities, req loom.ChatRequest) (*loom.ChatResponse, error) {
				calls++
				if phase == "default" || (phase == "object" && req.ResponseFormat != "") || (phase == "schema" && req.StructuredOutput != nil) {
					cancel()
				}
				if req.StructuredOutput != nil {
					return nil, context.Canceled
				}
				if req.ResponseFormat != "" {
					return &loom.ChatResponse{Content: `[]`, FinishReason: loom.FinishReasonStop}, nil
				}
				return reasoningResponse(2), nil
			}}
			r, err := Probe(ctx, b, Options{})
			wantCalls := map[string]int{"default": 1, "object": 4, "schema": 5}[phase]
			if !errors.Is(err, context.Canceled) || calls != wantCalls || len(r.Checks) != wantCalls || r.Model == "" {
				t.Fatalf("report=%+v calls=%d error=%v", r, calls, err)
			}
			if r.Coverage.StructuredOutput {
				t.Fatalf("incomplete checks claimed coverage: %+v", r)
			}
			if phase != "default" && r.Checks[3].Outcome != OutcomeNegative {
				t.Fatalf("confirmed Object failure lost: %+v", r)
			}
		})
	}
}
