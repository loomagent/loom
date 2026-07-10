package loom

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeStructuredModel struct {
	capabilities ModelCapabilities
	responses    []string
	requests     []ChatRequest
}

func (m *fakeStructuredModel) Name() string { return "fake/model" }

func (m *fakeStructuredModel) Capabilities() ModelCapabilities { return m.capabilities }

func (m *fakeStructuredModel) Chat(_ context.Context, req ChatRequest) (*ChatResponse, error) {
	m.requests = append(m.requests, req)
	if len(m.responses) == 0 {
		return nil, errors.New("no response")
	}
	content := m.responses[0]
	m.responses = m.responses[1:]
	return &ChatResponse{Content: content, FinishReason: FinishReasonStop}, nil
}

func (m *fakeStructuredModel) Stream(context.Context, ChatRequest) (Stream, error) {
	return nil, io.EOF
}

type structuredReviewFixture struct {
	OverallDone bool   `json:"overall_done" jsonschema:"是否已经完成评审"`
	Notes       string `json:"notes" jsonschema:"评审备注"`
}

func TestChatStructured_JSONSchemaRetryThenSuccess(t *testing.T) {
	model := &fakeStructuredModel{
		capabilities: ModelCapabilities{StructuredOutput: StructuredOutputJSONSchema},
		responses: []string{
			`{"overall_done":"no","notes":"bad type"}`,
			`{"overall_done":true,"notes":"ok"}`,
		},
	}
	got, resp, err := ChatStructured[structuredReviewFixture](
		context.Background(),
		"test.structured",
		model,
		ChatRequest{Messages: []Message{{Role: RoleUser, Content: "review"}}},
		WithStructuredName[structuredReviewFixture]("review-result"),
		WithStructuredDescription[structuredReviewFixture]("review decision"),
	)
	if err != nil {
		t.Fatalf("ChatStructured() error = %v", err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatalf("resp missing")
	}
	if !got.OverallDone || got.Notes != "ok" {
		t.Fatalf("got = %#v", got)
	}
	if len(model.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(model.requests))
	}
	first := model.requests[0]
	if first.StructuredOutput == nil {
		t.Fatalf("StructuredOutput missing")
	}
	if first.StructuredOutput.Mode != StructuredOutputJSONSchema {
		t.Fatalf("mode = %s", first.StructuredOutput.Mode)
	}
	if first.ResponseFormat != ResponseFormatDefault {
		t.Fatalf("response format = %s", first.ResponseFormat)
	}
	if len(model.requests[1].Messages) <= len(first.Messages) {
		t.Fatalf("retry request should include correction messages")
	}
}

func TestChatStructured_JSONObjectAddsSchemaPrompt(t *testing.T) {
	model := &fakeStructuredModel{
		capabilities: ModelCapabilities{StructuredOutput: StructuredOutputJSONObject},
		responses:    []string{`{"overall_done":false,"notes":"need more"}`},
	}
	got, _, err := ChatStructured[structuredReviewFixture](
		context.Background(),
		"test.structured",
		model,
		ChatRequest{Messages: []Message{{Role: RoleUser, Content: "review"}}},
	)
	if err != nil {
		t.Fatalf("ChatStructured() error = %v", err)
	}
	if got.OverallDone || got.Notes != "need more" {
		t.Fatalf("got = %#v", got)
	}
	if len(model.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(model.requests))
	}
	req := model.requests[0]
	if req.StructuredOutput == nil || req.StructuredOutput.Mode != StructuredOutputJSONObject {
		t.Fatalf("structured output = %#v", req.StructuredOutput)
	}
	if req.ResponseFormat != ResponseFormatJSONObject {
		t.Fatalf("response format = %s", req.ResponseFormat)
	}
	if len(req.Messages) != 2 || !strings.Contains(req.Messages[1].Content, "JSON Schema") {
		t.Fatalf("schema prompt missing: %#v", req.Messages)
	}
}

func TestChatStructured_FailoverRebuildsRequestForFallbackCapabilities(t *testing.T) {
	primary := &fakeCallModel{
		name:         "ark/primary",
		capabilities: ModelCapabilities{StructuredOutput: StructuredOutputJSONSchema},
		responses: []*ChatResponse{{
			Content:      `{"overall_done":false,"notes":"blocked"}`,
			FinishReason: FinishReasonContentFilter,
		}},
	}
	fallback := &fakeCallModel{
		name:         "deepseek/fallback",
		capabilities: ModelCapabilities{StructuredOutput: StructuredOutputJSONObject},
		responses: []*ChatResponse{{
			Content:      `{"overall_done":true,"notes":"ok"}`,
			FinishReason: FinishReasonStop,
		}},
	}

	got, _, err := ChatStructured[structuredReviewFixture](
		context.Background(),
		"test.structured_failover",
		primary,
		ChatRequest{Messages: []Message{{Role: RoleUser, Content: "review"}}},
		WithStructuredFailover[structuredReviewFixture](FailoverConfig{
			ShouldFailover: ShouldFailoverOnErrorOrFinishReason(FinishReasonContentFilter),
			GetFailoverModel: func(context.Context, FailoverAttempt) (ChatModel, error) {
				return fallback, nil
			},
		}),
	)
	if err != nil {
		t.Fatalf("ChatStructured() error = %v", err)
	}
	if !got.OverallDone || got.Notes != "ok" {
		t.Fatalf("got = %#v", got)
	}
	if len(primary.requests) != 1 || len(fallback.requests) != 1 {
		t.Fatalf("requests primary=%d fallback=%d", len(primary.requests), len(fallback.requests))
	}
	if primary.requests[0].StructuredOutput == nil || primary.requests[0].StructuredOutput.Mode != StructuredOutputJSONSchema {
		t.Fatalf("primary structured output = %#v", primary.requests[0].StructuredOutput)
	}
	if fallback.requests[0].StructuredOutput == nil || fallback.requests[0].StructuredOutput.Mode != StructuredOutputJSONObject {
		t.Fatalf("fallback structured output = %#v", fallback.requests[0].StructuredOutput)
	}
	if fallback.requests[0].ResponseFormat != ResponseFormatJSONObject {
		t.Fatalf("fallback response format = %s", fallback.requests[0].ResponseFormat)
	}
	if len(fallback.requests[0].Messages) != 2 || !strings.Contains(fallback.requests[0].Messages[1].Content, "JSON Schema") {
		t.Fatalf("fallback schema prompt missing: %#v", fallback.requests[0].Messages)
	}
}

func TestNormalizeStructuredOutputName(t *testing.T) {
	got := NormalizeStructuredOutputName("  review result / 中文  ")
	if got != "review_result" {
		t.Fatalf("got %q", got)
	}
}
