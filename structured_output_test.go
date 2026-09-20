// Verify strict structured responses, contract constraints, retries, and provider fallback.
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

// reviewContract mirrors what used to be a struct with json/jsonschema tags:
// the same declared contract used for tool arguments, now constraining model
// output.
func reviewContract() (contract *ArgsContract, done *BoolArg, notes *StringArg) {
	done = Bool("overall_done").Required().Desc("是否已经完成评审")
	notes = String("notes").Required().Desc("评审备注")
	return MustArgsContract("structured_review_fixture", done, notes), done, notes
}

func TestChatStructuredArgs_JSONSchemaRetryThenSuccess(t *testing.T) {
	contract, done, notes := reviewContract()
	model := &fakeStructuredModel{
		capabilities: ModelCapabilities{StructuredOutput: StructuredOutputJSONSchema},
		responses: []string{
			`{"overall_done":"no","notes":"bad type"}`,
			`{"overall_done":true,"notes":"ok"}`,
		},
	}
	got, resp, err := ChatStructuredArgs(
		context.Background(),
		"test.structured",
		model,
		ChatRequest{Messages: []Message{{Role: RoleUser, Content: "review"}}},
		contract,
		WithStructuredName("review-result"),
		WithStructuredDescription("review decision"),
	)
	if err != nil {
		t.Fatalf("ChatStructuredArgs() error = %v", err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatalf("resp missing")
	}
	if !done.Get(got) || notes.Get(got) != "ok" {
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

func TestChatStructuredArgs_JSONObjectAddsSchemaPrompt(t *testing.T) {
	contract, done, notes := reviewContract()
	model := &fakeStructuredModel{
		capabilities: ModelCapabilities{StructuredOutput: StructuredOutputJSONObject},
		responses:    []string{`{"overall_done":false,"notes":"need more"}`},
	}
	got, _, err := ChatStructuredArgs(
		context.Background(),
		"test.structured",
		model,
		ChatRequest{Messages: []Message{{Role: RoleUser, Content: "review"}}},
		contract,
	)
	if err != nil {
		t.Fatalf("ChatStructuredArgs() error = %v", err)
	}
	if done.Get(got) || notes.Get(got) != "need more" {
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

func TestChatStructuredArgs_FailoverRebuildsRequestForFallbackCapabilities(t *testing.T) {
	contract, done, notes := reviewContract()
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

	got, _, err := ChatStructuredArgs(
		context.Background(),
		"test.structured_failover",
		primary,
		ChatRequest{Messages: []Message{{Role: RoleUser, Content: "review"}}},
		contract,
		WithStructuredCallOptions(WithCallModelCaptureContent(false)),
		WithStructuredFailover(FailoverConfig{
			ShouldFailover: ShouldFailoverOnErrorOrFinishReason(FinishReasonContentFilter),
			GetFailoverModel: func(context.Context, FailoverAttempt) (ChatModel, error) {
				return fallback, nil
			},
		}),
	)
	if err != nil {
		t.Fatalf("ChatStructuredArgs() error = %v", err)
	}
	if !done.Get(got) || notes.Get(got) != "ok" {
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

func TestChatStructuredArgsProjectsContractConstraints(t *testing.T) {
	notes := String("notes").Required().MinLen(1).MaxLen(3)
	contract := MustArgsContract("structured_notes", notes)
	for _, mode := range []StructuredOutputMode{StructuredOutputJSONSchema, StructuredOutputJSONObject} {
		t.Run(string(mode), func(t *testing.T) {
			model := &fakeStructuredModel{
				capabilities: ModelCapabilities{StructuredOutput: mode},
				responses:    []string{`{"notes":"too long"}`, `{"notes":"ok"}`},
			}
			// Provider constraints and local validation must use the same schema.
			got, _, err := ChatStructuredArgs(t.Context(), "test.tags", model, ChatRequest{}, contract)
			if err != nil || notes.Get(got) != "ok" || len(model.requests) != 2 {
				t.Fatalf("contract validation: got=%v err=%v calls=%d", notes.Get(got), err, len(model.requests))
			}
			req := model.requests[0]
			if req.StructuredOutput == nil || req.StructuredOutput.Mode != mode {
				t.Fatalf("unexpected provider mode: %+v", req.StructuredOutput)
			}
			if mode == StructuredOutputJSONSchema {
				property := req.StructuredOutput.Schema.Properties["notes"]
				if property.MinLength == nil || *property.MinLength != 1 || property.MaxLength == nil || *property.MaxLength != 3 {
					t.Fatalf("provider schema lost contract constraints: %+v", property)
				}
			} else if len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Content, `"maxLength": 3`) {
				t.Fatalf("JSON object prompt lost schema constraints: %+v", req.Messages)
			}
		})
	}
}

func TestChatStructuredArgsRejectsInvalidResponsesByDefault(t *testing.T) {
	notes := String("notes").Required()
	contract := MustArgsContract("structured_strict", notes)
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
				got, _, err := ChatStructuredArgs(t.Context(), "test.strict", model, ChatRequest{}, contract)
				if err != nil || notes.Get(got) != "ok" || len(model.requests) != 2 {
					t.Fatalf("strict retry: got=%q err=%v calls=%d", notes.Get(got), err, len(model.requests))
				}
				if len(model.requests[1].Messages) <= len(model.requests[0].Messages) {
					t.Fatal("retry must include feedback to correct the invalid response")
				}
			})
		}
	}
}

func TestChatStructuredArgsInvalidResponseRespectsAttemptLimit(t *testing.T) {
	contract, _, _ := reviewContract()
	content := "```json\n{\"overall_done\":true,\"notes\":\"ok\"}\n```"
	model := &fakeStructuredModel{capabilities: ModelCapabilities{StructuredOutput: StructuredOutputJSONObject}, responses: []string{content}}
	_, response, err := ChatStructuredArgs(t.Context(), "test.strict_limit", model, ChatRequest{}, contract,
		WithStructuredMaxAttempts(1))
	var outputErr *StructuredOutputError
	if !errors.As(err, &outputErr) || outputErr.Attempt != 1 || outputErr.Content != content || response == nil || response.Content != content || len(model.requests) != 1 {
		t.Fatalf("expected final invalid response and structured error: response=%+v err=%v calls=%d", response, err, len(model.requests))
	}
}

// Schema checks must run before business rules, including for providers claiming native schema support.
func TestChatStructuredArgsValidatesSchemaBeforeBusinessRules(t *testing.T) {
	contract, done, notes := reviewContract()
	for _, mode := range []StructuredOutputMode{StructuredOutputJSONSchema, StructuredOutputJSONObject} {
		t.Run(string(mode), func(t *testing.T) {
			model := &fakeStructuredModel{capabilities: ModelCapabilities{StructuredOutput: mode}, responses: []string{
				`{"notes":"missing boolean"}`,
				`{"overall_done":false,"notes":"ok"}`,
			}}
			validations := 0
			got, _, err := ChatStructuredArgs(t.Context(), "test.validation_order", model, ChatRequest{}, contract,
				WithStructuredValidator(func(value Args) error {
					validations++
					if done.Get(value) || notes.Get(value) != "ok" {
						t.Errorf("business validator received unexpected data")
					}
					return nil
				}))
			if err != nil || done.Get(got) || notes.Get(got) != "ok" || validations != 1 || len(model.requests) != 2 {
				t.Fatalf("validation order: got=%v/%q err=%v validations=%d calls=%d", done.Get(got), notes.Get(got), err, validations, len(model.requests))
			}
		})
	}
}
