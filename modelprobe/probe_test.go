package modelprobe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/loomagent/loom"
)

var errUnsupported = errors.New("unsupported parameter")

type fakeBuilder struct {
	capabilities []loom.ModelCapabilities
	handler      func(loom.ModelCapabilities, loom.ChatRequest) (*loom.ChatResponse, error)
}

func (b *fakeBuilder) Build(_ context.Context, capabilities loom.ModelCapabilities) (loom.ChatModel, error) {
	b.capabilities = append(b.capabilities, capabilities)
	return &fakeModel{capabilities: capabilities, handler: b.handler}, nil
}

type fakeModel struct {
	capabilities loom.ModelCapabilities
	handler      func(loom.ModelCapabilities, loom.ChatRequest) (*loom.ChatResponse, error)
}

func (m *fakeModel) Name() string                         { return "fake/model" }
func (m *fakeModel) Capabilities() loom.ModelCapabilities { return m.capabilities }
func (m *fakeModel) Chat(_ context.Context, request loom.ChatRequest) (*loom.ChatResponse, error) {
	return m.handler(m.capabilities, request)
}
func (m *fakeModel) Stream(context.Context, loom.ChatRequest) (loom.Stream, error) {
	return nil, io.EOF
}

func TestProbeBuildsSyntheticModelsAndDerivesCapabilities(t *testing.T) {
	builder := &fakeBuilder{handler: func(capabilities loom.ModelCapabilities, request loom.ChatRequest) (*loom.ChatResponse, error) {
		if request.StructuredOutput != nil || request.ResponseFormat == loom.ResponseFormatJSONObject {
			return &loom.ChatResponse{Content: `{"ok":true}`, FinishReason: loom.FinishReasonStop}, nil
		}
		if capabilities.Reasoning == loom.ReasoningSupportNone {
			return reasoningResponse(5), nil
		}
		if request.Reasoning.Mode == loom.ReasoningModeDisabled {
			return reasoningResponse(0), nil
		}
		switch request.Reasoning.Effort {
		case loom.ReasoningEffortMedium:
			return nil, errUnsupported
		case loom.ReasoningEffortMax:
			return reasoningResponse(0), nil
		default:
			return reasoningResponse(7), nil
		}
	}}
	report, err := Probe(context.Background(), builder, Options{ErrorClassifier: func(err error) ErrorDisposition {
		if errors.Is(err, errUnsupported) {
			return ErrorUnsupported
		}
		return ErrorInconclusive
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(builder.capabilities) != 2 || builder.capabilities[0].Reasoning != loom.ReasoningSupportNone ||
		builder.capabilities[1].Reasoning != "" || builder.capabilities[1].StructuredOutput != "" ||
		len(builder.capabilities[1].ReasoningEfforts) != 0 {
		t.Fatalf("builder capabilities = %+v", builder.capabilities)
	}
	if report.SchemaVersion != 1 || report.Model != "fake/model" {
		t.Fatalf("report identity = %+v", report)
	}
	if report.Observed.Reasoning != loom.ReasoningSupportToggleableDefaultOn || !report.Coverage.ReasoningSupport {
		t.Fatalf("reasoning = %q coverage=%+v", report.Observed.Reasoning, report.Coverage)
	}
	wantEfforts := []loom.ReasoningEffort{loom.ReasoningEffortLow, loom.ReasoningEffortHigh}
	if !report.Coverage.AcceptedReasoningEfforts || !slices.Equal(report.Observed.AcceptedReasoningEfforts, wantEfforts) {
		t.Fatalf("efforts = %v coverage=%+v", report.Observed.AcceptedReasoningEfforts, report.Coverage)
	}
	if report.Observed.StructuredOutput != loom.StructuredOutputJSONSchema || !report.Coverage.StructuredOutput {
		t.Fatalf("structured output = %q coverage=%+v", report.Observed.StructuredOutput, report.Coverage)
	}
}

func TestProbeDoesNotTurnOperationalFailureIntoUnsupported(t *testing.T) {
	builder := &fakeBuilder{handler: func(capabilities loom.ModelCapabilities, request loom.ChatRequest) (*loom.ChatResponse, error) {
		if request.StructuredOutput != nil || request.ResponseFormat != "" {
			return &loom.ChatResponse{Content: `{"ok":true}`}, nil
		}
		if capabilities.Reasoning == loom.ReasoningSupportNone {
			return reasoningResponse(0), nil
		}
		if request.Reasoning.Mode == loom.ReasoningModeEnabled {
			return nil, errors.New("upstream unavailable")
		}
		return reasoningResponse(0), nil
	}}
	report, err := Probe(context.Background(), builder, Options{ReasoningEfforts: []loom.ReasoningEffort{}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Coverage.ReasoningSupport || report.Observed.Reasoning != "" {
		t.Fatalf("operational error produced reasoning capability: %+v", report)
	}
	declared := loom.ModelCapabilities{Reasoning: loom.ReasoningSupportNone, StructuredOutput: loom.StructuredOutputJSONSchema}
	if mismatches := Compare(declared, report); len(mismatches) != 0 {
		t.Fatalf("unknown mismatch: %+v", mismatches)
	}
}

func TestProbeSchemaRejectsNonDiscriminatingOutput(t *testing.T) {
	model := &fakeModel{handler: func(_ loom.ModelCapabilities, request loom.ChatRequest) (*loom.ChatResponse, error) {
		if request.StructuredOutput != nil {
			return &loom.ChatResponse{Content: `{"ok":false}`}, nil
		}
		return &loom.ChatResponse{Content: `[]`}, nil
	}}
	object, schema, err := probeStructuredOutput(context.Background(), model, defaultPerCallTimeout, loom.Reasoning{Mode: loom.ReasoningModeDisabled}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if object.Outcome != OutcomeNegative || schema.Outcome != OutcomeNegative {
		t.Fatalf("object=%+v schema=%+v", object, schema)
	}
}

func TestDeriveReasoningSupport(t *testing.T) {
	tests := []struct {
		defaultOn, enable, disable bool
		want                       loom.ReasoningSupport
	}{
		{true, true, true, loom.ReasoningSupportToggleableDefaultOn},
		{false, true, true, loom.ReasoningSupportToggleableDefaultOff},
		{true, true, false, loom.ReasoningSupportAlwaysOn},
		{false, false, false, loom.ReasoningSupportNone},
	}
	for _, test := range tests {
		if got := DeriveReasoningSupport(test.defaultOn, test.enable, test.disable); got != test.want {
			t.Fatalf("derive(%v,%v,%v)=%q want %q", test.defaultOn, test.enable, test.disable, got, test.want)
		}
	}
}

func TestReportJSONUsesStableFieldNames(t *testing.T) {
	report := Report{
		SchemaVersion: 1,
		Observed: ObservedCapabilities{
			Reasoning:                loom.ReasoningSupportAlwaysOn,
			AcceptedReasoningEfforts: []loom.ReasoningEffort{loom.ReasoningEffortHigh},
			StructuredOutput:         loom.StructuredOutputJSONSchema,
		},
		Checks: []Check{},
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, field := range []string{`"schema_version":1`, `"accepted_reasoning_efforts":["high"]`, `"structured_output":"json_schema"`, `"checks":[]`} {
		if !strings.Contains(text, field) {
			t.Fatalf("JSON %s missing %s", text, field)
		}
	}
	if strings.Contains(text, "ReasoningEfforts") {
		t.Fatalf("unstable Go field name in JSON: %s", text)
	}
}

func reasoningResponse(tokens uint64) *loom.ChatResponse {
	return &loom.ChatResponse{Content: "2", Usage: loom.Usage{ReasoningTokens: tokens}, FinishReason: loom.FinishReasonStop}
}
