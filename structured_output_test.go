// Verify strict structured responses, contract constraints, retries, and provider fallback.
package loom

import (
	"context"
	jsonv2 "encoding/json/v2"
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
	done = Bool("overall_done").Required().Desc("whether the review is complete")
	notes = String("notes").Required().Desc("review notes")
	return MustArgsContract("structured_review_fixture", done, notes), done, notes
}

// One call per invocation. What to do about an invalid response — how many more times to ask, and
// what to send with it — is the caller's policy, so the failure comes back instead of being
// retried here with feedback Loom wrote.
func TestChatStructuredArgsReportsInvalidOutputWithoutRetrying(t *testing.T) {
	contract, _, _ := reviewContract()
	invalid := `{"overall_done":"no","notes":"bad type"}`
	model := &fakeStructuredModel{
		capabilities: ModelCapabilities{StructuredOutput: StructuredOutputJSONSchema},
		responses:    []string{invalid, `{"overall_done":true,"notes":"ok"}`},
	}
	_, resp, err := ChatStructuredArgs(
		t.Context(),
		"test.structured",
		model,
		ChatRequest{Messages: []Message{{Role: RoleUser, Content: "review"}}},
		contract,
		WithStructuredName("review-result"),
		WithStructuredDescription("review decision"),
	)
	var outputErr *StructuredOutputError
	if !errors.As(err, &outputErr) {
		t.Fatalf("error = %v, want a StructuredOutputError", err)
	}
	if outputErr.Content != invalid || resp == nil || resp.Content != invalid {
		t.Fatalf("the invalid response must come back with the error: resp=%+v err=%+v", resp, outputErr)
	}
	if len(model.requests) != 1 {
		t.Fatalf("requests = %d, want 1: the second response must stay unused", len(model.requests))
	}
	first := model.requests[0]
	if first.StructuredOutput == nil || first.StructuredOutput.Mode != StructuredOutputJSONSchema ||
		first.StructuredOutput.Name != "review-result" || first.StructuredOutput.Description != "review decision" {
		t.Fatalf("structured output = %+v", first.StructuredOutput)
	}
	if first.ResponseFormat != ResponseFormatDefault {
		t.Fatalf("response format = %s", first.ResponseFormat)
	}
	if len(first.Messages) != 1 || first.Messages[0].Content != "review" {
		t.Fatalf("messages = %+v, want the caller's", first.Messages)
	}
}

// The rules belong in the structured-output field, never in the conversation: a prompt the caller
// did not write is a hidden request. This covers every capability, including the undeclared one.
func TestChatStructuredArgsDoesNotTouchThePrompt(t *testing.T) {
	contract, done, notes := reviewContract()
	caller := []Message{{Role: RoleUser, Content: "review"}}
	for _, mode := range []StructuredOutputMode{StructuredOutputJSONSchema, StructuredOutputJSONObject, StructuredOutputUnsupported} {
		t.Run(string(mode), func(t *testing.T) {
			model := &fakeStructuredModel{
				capabilities: ModelCapabilities{StructuredOutput: mode},
				responses:    []string{`{"overall_done":false,"notes":"need more"}`},
			}
			got, _, err := ChatStructuredArgs(
				t.Context(),
				"test.structured",
				model,
				ChatRequest{Messages: caller},
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
			sent, err := jsonv2.Marshal(model.requests[0].Messages)
			if err != nil {
				t.Fatal(err)
			}
			want, err := jsonv2.Marshal(caller)
			if err != nil {
				t.Fatal(err)
			}
			if string(sent) != string(want) {
				t.Fatalf("messages = %s, want exactly the caller's %s", sent, want)
			}
		})
	}
}

// A model that declares it cannot produce structured output is a contradiction with this call,
// and saying so is better than calling it and hoping the conversation compensates.
func TestChatStructuredArgsRejectsAModelWithoutStructuredOutput(t *testing.T) {
	contract, _, _ := reviewContract()
	model := &fakeStructuredModel{
		capabilities: ModelCapabilities{StructuredOutput: StructuredOutputNone},
		responses:    []string{`{"overall_done":true,"notes":"ok"}`},
	}
	_, _, err := ChatStructuredArgs(
		t.Context(),
		"test.structured_none",
		model,
		ChatRequest{Messages: []Message{{Role: RoleUser, Content: "review"}}},
		contract,
	)
	if err == nil || !strings.Contains(err.Error(), "structured_output") {
		t.Fatalf("error = %v, want a complaint about the declared structured output", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("requests = %d, want none: the call must not go out", len(model.requests))
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
	if len(fallback.requests[0].Messages) != 1 || fallback.requests[0].Messages[0].Content != "review" {
		t.Fatalf("fallback must receive the caller's messages untouched: %#v", fallback.requests[0].Messages)
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
			// Provider constraints and local validation must use the same schema, so a value the
			// schema rejects is rejected here too, in both modes.
			tooLong := &fakeStructuredModel{
				capabilities: ModelCapabilities{StructuredOutput: mode},
				responses:    []string{`{"notes":"too long"}`},
			}
			if _, _, err := ChatStructuredArgs(t.Context(), "test.tags", tooLong, ChatRequest{}, contract); err == nil {
				t.Fatal("a value that violates maxLength must be rejected")
			}
			if len(tooLong.requests) != 1 {
				t.Fatalf("requests = %d, want 1", len(tooLong.requests))
			}
			model := &fakeStructuredModel{
				capabilities: ModelCapabilities{StructuredOutput: mode},
				responses:    []string{`{"notes":"ok"}`},
			}
			got, _, err := ChatStructuredArgs(t.Context(), "test.tags", model, ChatRequest{}, contract)
			if err != nil || notes.Get(got) != "ok" || len(model.requests) != 1 {
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
			} else if req.ResponseFormat != ResponseFormatJSONObject || len(req.Messages) != 0 {
				t.Fatalf("json_object must send no schema and add no messages: format=%s messages=%+v", req.ResponseFormat, req.Messages)
			}
		})
	}
}

func TestChatStructuredArgsRejectsInvalidResponsesByDefault(t *testing.T) {
	notes := String("notes").Required()
	contract := MustArgsContract("structured_strict", notes)
	for _, mode := range []StructuredOutputMode{StructuredOutputJSONSchema, StructuredOutputJSONObject} {
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
				got, resp, err := ChatStructuredArgs(t.Context(), "test.strict", model, ChatRequest{}, contract)
				var outputErr *StructuredOutputError
				if !errors.As(err, &outputErr) {
					t.Fatalf("err = %v, want a StructuredOutputError", err)
				}
				if resp == nil || resp.Content != tc.response || outputErr.Content != tc.response {
					t.Fatalf("the rejected response must come back: resp=%+v err=%+v", resp, outputErr)
				}
				if got.JSON() != nil {
					t.Fatalf("no arguments may be returned for a rejected response: %s", got.JSON())
				}
				if len(model.requests) != 1 {
					t.Fatalf("requests = %d, want 1", len(model.requests))
				}
			})
		}
	}
}

// A truncated response is reported like any other invalid one: whether a longer budget is worth
// another call is the caller's decision.
func TestChatStructuredArgsReportsATruncatedResponse(t *testing.T) {
	contract, _, _ := reviewContract()
	model := &fakeCallModel{
		name:         "fake/truncated",
		capabilities: ModelCapabilities{StructuredOutput: StructuredOutputJSONSchema},
		responses:    []*ChatResponse{{Content: `{"overall_done":true,"notes":"cut`, FinishReason: FinishReasonLength}},
	}
	_, resp, err := ChatStructuredArgs(t.Context(), "test.truncated", model, ChatRequest{}, contract)
	var outputErr *StructuredOutputError
	if !errors.As(err, &outputErr) || !strings.Contains(err.Error(), string(FinishReasonLength)) {
		t.Fatalf("err = %v, want a structured error naming %s", err, FinishReasonLength)
	}
	if resp == nil || resp.FinishReason != FinishReasonLength || len(model.requests) != 1 {
		t.Fatalf("resp=%+v requests=%d", resp, len(model.requests))
	}
}

// Schema checks must run before business rules, including for providers claiming native schema support.
func TestChatStructuredArgsValidatesSchemaBeforeBusinessRules(t *testing.T) {
	contract, done, notes := reviewContract()
	validator := func(validations *int) StructuredOption {
		return WithStructuredValidator(func(value Args) error {
			*validations++
			if done.Get(value) || notes.Get(value) != "ok" {
				t.Errorf("business validator received unexpected data")
			}
			return nil
		})
	}
	for _, mode := range []StructuredOutputMode{StructuredOutputJSONSchema, StructuredOutputJSONObject} {
		t.Run(string(mode), func(t *testing.T) {
			// A schema-invalid response never reaches the business rules.
			invalid := &fakeStructuredModel{capabilities: ModelCapabilities{StructuredOutput: mode},
				responses: []string{`{"notes":"missing boolean"}`}}
			validations := 0
			if _, _, err := ChatStructuredArgs(t.Context(), "test.validation_order", invalid, ChatRequest{}, contract, validator(&validations)); err == nil {
				t.Fatal("a schema-invalid response must be rejected")
			}
			if validations != 0 {
				t.Fatalf("business rules ran %d times on a schema-invalid response, want 0", validations)
			}
			// A schema-valid one does, exactly once.
			valid := &fakeStructuredModel{capabilities: ModelCapabilities{StructuredOutput: mode},
				responses: []string{`{"overall_done":false,"notes":"ok"}`}}
			got, _, err := ChatStructuredArgs(t.Context(), "test.validation_order", valid, ChatRequest{}, contract, validator(&validations))
			if err != nil || done.Get(got) || notes.Get(got) != "ok" || validations != 1 || len(valid.requests) != 1 {
				t.Fatalf("validation order: got=%v/%q err=%v validations=%d calls=%d", done.Get(got), notes.Get(got), err, validations, len(valid.requests))
			}
		})
	}
}
