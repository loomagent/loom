// Verify strict structured responses, schema constraints, retries, and provider fallback.
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
		WithStructuredCallOptions[structuredReviewFixture](WithCallModelCaptureContent(false)),
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

func TestChatStructuredProjectsValidationTags(t *testing.T) {
	type output struct {
		Notes string `json:"notes" validate:"min=1,max=3"`
	}
	for _, mode := range []StructuredOutputMode{StructuredOutputJSONSchema, StructuredOutputJSONObject} {
		t.Run(string(mode), func(t *testing.T) {
			model := &fakeStructuredModel{
				capabilities: ModelCapabilities{StructuredOutput: mode},
				responses:    []string{`{"notes":"too long"}`, `{"notes":"ok"}`},
			}
			// Provider constraints and local validation must use the same schema.
			got, _, err := ChatStructured[output](t.Context(), "test.tags", model, ChatRequest{})
			if err != nil || got.Notes != "ok" || len(model.requests) != 2 {
				t.Fatalf("tag validation: got=%+v err=%v calls=%d", got, err, len(model.requests))
			}
			req := model.requests[0]
			if req.StructuredOutput == nil || req.StructuredOutput.Mode != mode {
				t.Fatalf("unexpected provider mode: %+v", req.StructuredOutput)
			}
			if mode == StructuredOutputJSONSchema {
				notes := req.StructuredOutput.Schema.Properties["notes"]
				if notes.MinLength == nil || *notes.MinLength != 1 || notes.MaxLength == nil || *notes.MaxLength != 3 {
					t.Fatalf("provider schema lost validate constraints: %+v", notes)
				}
			} else if len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Content, `"maxLength": 3`) {
				t.Fatalf("JSON object prompt lost schema constraints: %+v", req.Messages)
			}
		})
	}
}

func TestChatStructuredRejectsInvalidResponsesByDefault(t *testing.T) {
	type output struct {
		Notes string `json:"notes"`
	}
	for _, mode := range []StructuredOutputMode{StructuredOutputJSONSchema, StructuredOutputJSONObject, StructuredOutputNone} {
		for _, tc := range []struct{ name, response string }{
			{"fenced", "```json\n{\"notes\":\"bad\"}\n```"},
			{"leading prose", `Here is the result: {"notes":"bad"}`},
			{"trailing prose", `{"notes":"bad"} End of result.`},
			{"multiple values", `{"notes":"bad"} {"notes":"bad"}`},
			{"duplicate field", `{"notes":"bad","notes":"ok"}`},
			{"null object", `null`},
			{"null field", `{"notes":null}`},
			{"truncated", `{"notes":`},
			{"empty", " "},
			{"wrong type", `{"notes":true}`},
			{"missing field", `{}`},
			{"extra field", `{"notes":"bad","extra":true}`},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				model := &fakeStructuredModel{capabilities: ModelCapabilities{StructuredOutput: mode},
					responses: []string{tc.response, " \n{\"notes\":\"ok\"}\t "}}
				got, _, err := ChatStructured[output](t.Context(), "test.strict", model, ChatRequest{})
				if err != nil || got.Notes != "ok" || len(model.requests) != 2 {
					t.Fatalf("strict retry: got=%+v err=%v calls=%d", got, err, len(model.requests))
				}
				if len(model.requests[1].Messages) <= len(model.requests[0].Messages) {
					t.Fatal("retry must include feedback to correct the invalid response")
				}
			})
		}
	}
}

func TestChatStructuredInvalidResponseRespectsAttemptLimit(t *testing.T) {
	content := "```json\n{\"overall_done\":true,\"notes\":\"ok\"}\n```"
	model := &fakeStructuredModel{capabilities: ModelCapabilities{StructuredOutput: StructuredOutputJSONObject}, responses: []string{content}}
	_, response, err := ChatStructured[structuredReviewFixture](t.Context(), "test.strict_limit", model, ChatRequest{},
		WithStructuredMaxAttempts[structuredReviewFixture](1))
	var outputErr *StructuredOutputError
	if !errors.As(err, &outputErr) || outputErr.Attempt != 1 || outputErr.Content != content || response == nil || response.Content != content || len(model.requests) != 1 {
		t.Fatalf("expected final invalid response and structured error: response=%+v err=%v calls=%d", response, err, len(model.requests))
	}
}

// Schema checks must run before business rules, including for providers claiming native schema support.
func TestChatStructuredValidatesSchemaBeforeBusinessRules(t *testing.T) {
	for _, mode := range []StructuredOutputMode{StructuredOutputJSONSchema, StructuredOutputJSONObject} {
		t.Run(string(mode), func(t *testing.T) {
			model := &fakeStructuredModel{capabilities: ModelCapabilities{StructuredOutput: mode}, responses: []string{
				`{"notes":"missing boolean"}`,
				`{"overall_done":false,"notes":"ok"}`,
			}}
			validations := 0
			got, _, err := ChatStructured[structuredReviewFixture](t.Context(), "test.validation_order", model, ChatRequest{},
				WithStructuredValidator(func(value structuredReviewFixture) error {
					validations++
					if value.OverallDone || value.Notes != "ok" {
						t.Errorf("business validator received unexpected data: %+v", value)
					}
					return nil
				}))
			if err != nil || got.OverallDone || got.Notes != "ok" || validations != 1 || len(model.requests) != 2 {
				t.Fatalf("validation order: got=%+v err=%v validations=%d calls=%d", got, err, validations, len(model.requests))
			}
		})
	}
}
