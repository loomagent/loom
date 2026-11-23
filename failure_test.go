package loom

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func newVendorFailureTool() Tool {
	return NewArgsTool(MustArgsContract("search"), "fails with provider details",
		func(context.Context, Args) (string, error) {
			return "", Fail(
				errVendorQuota,
				"the search tool is temporarily unavailable",
				PublicFacing{Code: "search_down", Message: "搜索服务暂时不可用，请稍后再试"},
			)
		},
	)
}

// errVendorQuota is the shared raw cause the vendor-failure tool wraps, so tests can
// assert errors.Is against the very instance.
var errVendorQuota = errors.New("vendor X upstream: 500 quota exceeded")

// ===== Fail / FailureOf =====

func TestFailureOf_FindsOutermostFailureError(t *testing.T) {
	t.Parallel()
	cause := errors.New("vendor X upstream: 500")
	inner := Fail(cause, "inner text", PublicFacing{Code: "inner", Message: "内层"})
	outer := Fail(inner, "outer text", PublicFacing{Code: "outer", Message: "外层"})

	wrapped := fmt.Errorf("step: %w", fmt.Errorf("tools: %w", outer))

	fe, ok := FailureOf(wrapped)
	if !ok {
		t.Fatal("FailureOf did not find a FailureError through two %w wrappings")
	}
	// The outermost FailureError is the one nearest the caller, which is the
	// boundary that supplied the final texts.
	if fe != outer {
		t.Fatalf("FailureOf returned %p, want the outer FailureError %p", fe, outer)
	}
	if fe.Model != "outer text" || fe.Public.Code != "outer" {
		t.Fatalf("outermost fields: %+v", fe)
	}
	if !errors.Is(wrapped, cause) || !errors.Is(wrapped, inner) {
		t.Fatal("the chain no longer reaches the wrapped errors")
	}

	if _, ok := FailureOf(errors.New("plain")); ok {
		t.Error("FailureOf matched a plain error")
	}
	if _, ok := FailureOf(fmt.Errorf("wrap: %w", errors.New("plain"))); ok {
		t.Error("FailureOf matched a wrapped plain error")
	}
	if _, ok := FailureOf(nil); ok {
		t.Error("FailureOf matched nil")
	}
}

func TestFailureError_ErrorText(t *testing.T) {
	t.Parallel()
	cause := errors.New("vendor X upstream: 500")
	if got := Fail(cause, "model text", PublicFacing{Code: "c", Message: "m"}).Error(); got != "vendor X upstream: 500" {
		t.Errorf("Error with cause = %q, want the cause text", got)
	}
	if got := Fail(nil, "model text", PublicFacing{Code: "c", Message: "m"}).Error(); got != "model text" {
		t.Errorf("Error without cause = %q, want the model text", got)
	}
}

func TestFailureItemError_EmptyTextsKeepRawError(t *testing.T) {
	t.Parallel()
	err := Fail(errors.New("raw detail"), "", PublicFacing{})
	itemErr := failureItemError("tool_failed", err)
	if itemErr.Message != "raw detail" || itemErr.Detail != "raw detail" {
		t.Errorf("message/detail: %+v", itemErr)
	}
	if itemErr.Public != nil {
		t.Errorf("zero PublicFacing must not become a pointer field: %+v", itemErr.Public)
	}
}

// ===== Tool failure → ItemError =====

func TestExecuteToolCalls_FailureErrorTexts(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(newVendorFailureTool())

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		results, err := ExecuteToolCalls(ctx, w, reg, []ToolCall{{ID: "c1", Name: "search", Arguments: "{}"}})
		if err != nil {
			return err
		}
		cause := errVendorQuota
		if !errors.Is(results[0].Err, cause) {
			t.Error("the returned error no longer reaches the cause")
		}
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: TextUserMessage("x")})

	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	itemErr := turn.Items[1].Error
	if itemErr == nil {
		t.Fatalf("tool_result has no error: %+v", turn.Items[1])
	}
	if itemErr.Code != "tool_failed" {
		t.Errorf("code = %q, want tool_failed", itemErr.Code)
	}
	if itemErr.Message != "the search tool is temporarily unavailable" {
		t.Errorf("message = %q, want the model-facing text", itemErr.Message)
	}
	if itemErr.Detail != "vendor X upstream: 500 quota exceeded" {
		t.Errorf("detail = %q, want the raw cause text", itemErr.Detail)
	}
	if itemErr.Public == nil || itemErr.Public.Code != "search_down" || itemErr.Public.Message != "搜索服务暂时不可用，请稍后再试" {
		t.Errorf("public: %+v", itemErr.Public)
	}
}

func TestExecuteToolCalls_PlainErrorUnchanged(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(newFailingTool())

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		_, err := ExecuteToolCalls(ctx, w, reg, []ToolCall{{ID: "c1", Name: "bad", Arguments: "{}"}})
		if err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: TextUserMessage("x")})

	itemErr := turn.Items[1].Error
	if itemErr == nil {
		t.Fatalf("tool_result has no error")
	}
	if itemErr.Code != "tool_failed" || itemErr.Message != "boom" || itemErr.Detail != "boom" {
		t.Errorf("item error: %+v", itemErr)
	}
	if itemErr.Public != nil {
		t.Errorf("plain error must not grow a public block: %+v", itemErr.Public)
	}
}

func TestExecuteToolCalls_InvalidArgumentsCode(t *testing.T) {
	reg := NewToolRegistry()
	value := String("value").Required().Desc("Value to echo back.")
	_ = reg.Register(NewArgsTool(MustArgsContract("echo", value), "echo back",
		func(_ context.Context, args Args) (string, error) {
			return `{"got":"` + value.Get(args) + `"}`, nil
		},
	))

	var results []ToolExecResult
	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		// the model omitted the required "value" argument
		var err error
		results, err = ExecuteToolCalls(ctx, w, reg, []ToolCall{{ID: "c1", Name: "echo", Arguments: `{}`}})
		if err != nil {
			return err
		}
		if argErr, ok := errors.AsType[*ToolArgumentError](results[0].Err); !ok || argErr.Tool != "echo" {
			t.Errorf("invoke error = %v, want *ToolArgumentError for tool echo", results[0].Err)
		}
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: TextUserMessage("x")})

	itemErr := turn.Items[1].Error
	if itemErr == nil {
		t.Fatalf("tool_result has no error")
	}
	if itemErr.Code != "invalid_arguments" {
		t.Errorf("code = %q, want invalid_arguments", itemErr.Code)
	}
	if !strings.Contains(itemErr.Message, "value") {
		t.Errorf("message = %q, want the raw argument guidance mentioning the field", itemErr.Message)
	}
	if itemErr.Detail != itemErr.Message {
		t.Errorf("detail = %q, want the same raw text as message", itemErr.Detail)
	}
	if itemErr.Public != nil {
		t.Errorf("argument rejection must not grow a public block: %+v", itemErr.Public)
	}
	// An unwrapped argument error keeps its verbatim guidance both in the stored
	// ItemError and in what the model is fed.
	raw := results[0].Err.Error()
	if itemErr.Message != raw {
		t.Errorf("message = %q, want the verbatim argument error %q", itemErr.Message, raw)
	}
	msgs := AppendAssistantTurn(nil, &ChatResponse{}, results)
	if len(msgs) != 2 {
		t.Fatalf("msgs: %+v", msgs)
	}
	if want := "tool execution error: " + raw; TextOf(msgs[1].Parts) != want {
		t.Errorf("fed content = %q, want %q", TextOf(msgs[1].Parts), want)
	}
}

// TestExecuteToolCalls_WrappedArgumentErrorKeepsModelText covers a product tool
// that wraps its ToolArgumentError in a FailureError: the code still classifies
// as invalid_arguments, and the stored ItemError.Message is exactly the text the
// model is fed — one modelText, no special case.
func TestExecuteToolCalls_WrappedArgumentErrorKeepsModelText(t *testing.T) {
	argErr := &ToolArgumentError{
		Tool:   "echo",
		Kind:   ToolArgumentErrorSchema,
		Issues: []ToolArgumentIssue{{Field: "value", Rule: "required", Message: "value is required"}},
	}
	reg := NewToolRegistry()
	_ = reg.Register(NewArgsTool(MustArgsContract("echo"), "echo back",
		func(context.Context, Args) (string, error) {
			return "", Fail(argErr, "fix the arguments and call again",
				PublicFacing{Code: "bad_arguments", Message: "参数有误，请调整后重试"})
		},
	))

	var results []ToolExecResult
	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		var err error
		results, err = ExecuteToolCalls(ctx, w, reg, []ToolCall{{ID: "c1", Name: "echo", Arguments: `{}`}})
		if err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: TextUserMessage("x")})

	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	itemErr := turn.Items[1].Error
	if itemErr == nil {
		t.Fatalf("tool_result has no error")
	}
	if itemErr.Code != "invalid_arguments" {
		t.Errorf("code = %q, want invalid_arguments", itemErr.Code)
	}
	if itemErr.Message != "fix the arguments and call again" {
		t.Errorf("message = %q, want the model-facing text", itemErr.Message)
	}
	if itemErr.Detail != argErr.Error() {
		t.Errorf("detail = %q, want the raw argument error text", itemErr.Detail)
	}
	if itemErr.Public == nil || itemErr.Public.Code != "bad_arguments" {
		t.Errorf("public: %+v", itemErr.Public)
	}
	msgs := AppendAssistantTurn(nil, &ChatResponse{}, results)
	if len(msgs) != 2 {
		t.Fatalf("msgs: %+v", msgs)
	}
	if want := "tool execution error: " + itemErr.Message; TextOf(msgs[1].Parts) != want {
		t.Errorf("fed content = %q, want the stored message %q", TextOf(msgs[1].Parts), want)
	}
	if strings.Contains(TextOf(msgs[1].Parts), "invalid arguments for tool") {
		t.Errorf("fed content = %q must not fall back to the raw argument error", TextOf(msgs[1].Parts))
	}
}

// ===== Model-facing tool message =====

// TestModelTextMatchesFedBackToolMessage is the invariant behind ItemError.Message
// and AppendAssistantTurn: whatever the error shape, the text the model is fed is
// "tool execution error: " plus exactly the stored message. Both go through
// modelText, so they cannot drift apart.
func TestModelTextMatchesFedBackToolMessage(t *testing.T) {
	t.Parallel()
	cases := map[string]error{
		"plain error":              errors.New("boom"),
		"failure error":            Fail(errVendorQuota, "the search tool is temporarily unavailable", PublicFacing{Code: "search_down"}),
		"failure with empty model": Fail(errVendorQuota, "", PublicFacing{Code: "search_down"}),
		"argument error": &ToolArgumentError{
			Tool:   "echo",
			Issues: []ToolArgumentIssue{{Field: "value", Rule: "required", Message: "value is required"}},
		},
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			itemErr := failureItemError("tool_failed", err)
			msgs := AppendAssistantTurn(nil, &ChatResponse{}, []ToolExecResult{{Call: ToolCall{ID: "c1"}, Err: err}})
			if len(msgs) != 2 || msgs[1].Role != RoleTool {
				t.Fatalf("msgs: %+v", msgs)
			}
			if want := "tool execution error: " + itemErr.Message; TextOf(msgs[1].Parts) != want {
				t.Errorf("fed content = %q, want the stored message %q", TextOf(msgs[1].Parts), want)
			}
		})
	}
}

func TestAppendAssistantTurn_FailureErrorModelText(t *testing.T) {
	t.Parallel()
	cause := errors.New("vendor X upstream: 500 quota exceeded")
	results := []ToolExecResult{{
		Call: ToolCall{ID: "c1", Name: "search"},
		Err:  Fail(cause, "the search tool is temporarily unavailable", PublicFacing{Code: "search_down", Message: "搜索暂时不可用"}),
	}}
	msgs := AppendAssistantTurn(nil, &ChatResponse{}, results)
	if len(msgs) != 2 || msgs[1].Role != RoleTool || msgs[1].ToolCallID != "c1" {
		t.Fatalf("msgs: %+v", msgs)
	}
	if !strings.Contains(TextOf(msgs[1].Parts), "the search tool is temporarily unavailable") {
		t.Errorf("content = %q, want it to carry the model-facing text", TextOf(msgs[1].Parts))
	}
	if strings.Contains(TextOf(msgs[1].Parts), "vendor X") {
		t.Errorf("content = %q, must not carry the raw cause text", TextOf(msgs[1].Parts))
	}
}

func TestAppendAssistantTurn_PlainErrorUnchanged(t *testing.T) {
	t.Parallel()
	results := []ToolExecResult{{Call: ToolCall{ID: "c1"}, Err: errors.New("boom")}}
	msgs := AppendAssistantTurn(nil, &ChatResponse{}, results)
	if TextOf(msgs[1].Parts) != "tool execution error: boom" {
		t.Errorf("content = %q, want the raw error text as before", TextOf(msgs[1].Parts))
	}
}

// ===== Handler failure → CloseReason =====

func TestRun_HandlerFailureErrorFillsCloseReasonPublic(t *testing.T) {
	cause := errors.New("worker panic: nil deref at vendor.go:42")
	handlerErr := Fail(fmt.Errorf("plan phase: %w", cause), "the task could not be finished",
		PublicFacing{Code: "task_failed", Message: "任务暂时无法完成，请稍后重试"})

	turn, err := Run(context.Background(), func(context.Context, TurnWriter, []Turn, UserMessage) error {
		return handlerErr
	}, RunOptions{ConversationID: "conv_test", Input: TextUserMessage("x")})

	if turn == nil || turn.Status != TurnStatusFailed || turn.CloseReason == nil {
		t.Fatalf("turn: %+v", turn)
	}
	cr := turn.CloseReason
	if cr.Code != CloseCodeAgentError {
		t.Errorf("code = %q, want agent_error", cr.Code)
	}
	// Message stays the raw text: FailureError.Error is the cause's text.
	if cr.Message != handlerErr.Error() || !strings.Contains(cr.Message, "worker panic") {
		t.Errorf("message = %q, want the raw error text", cr.Message)
	}
	if cr.Public == nil || cr.Public.Code != "task_failed" || cr.Public.Message != "任务暂时无法完成，请稍后重试" {
		t.Errorf("public: %+v", cr.Public)
	}
	if !errors.Is(cr.Cause, cause) || !errors.Is(err, cause) {
		t.Error("the close reason and Run's error no longer reach the cause")
	}
}

func TestRun_PlainHandlerErrorHasNoPublic(t *testing.T) {
	turn, _ := Run(context.Background(), func(context.Context, TurnWriter, []Turn, UserMessage) error {
		return errors.New("boom")
	}, RunOptions{ConversationID: "conv_test", Input: TextUserMessage("x")})

	if cr := turn.CloseReason; cr == nil || cr.Message != "boom" || cr.Public != nil {
		t.Errorf("close reason: %+v", cr)
	}
}

// ===== Step and streaming closures =====

func TestStep_FailureErrorTexts(t *testing.T) {
	cause := errors.New("vendor X upstream: refused")
	stepErr := Fail(cause, "the lookup step failed", PublicFacing{Code: "lookup_failed", Message: "查询失败"})

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		_ = w.Step(ctx, "lookup", func(context.Context, Step) error { return stepErr })
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: TextUserMessage("x")})

	step := turn.Items[0]
	if step.Kind != ItemKindStep || step.Status != ItemStatusFailed || step.Error == nil {
		t.Fatalf("step item: %+v", step)
	}
	if step.Error.Code != "step_failed" {
		t.Errorf("code = %q, want step_failed", step.Error.Code)
	}
	if step.Error.Message != "the lookup step failed" {
		t.Errorf("message = %q, want the model-facing text", step.Error.Message)
	}
	if step.Error.Detail != "vendor X upstream: refused" {
		t.Errorf("detail = %q, want the raw cause text", step.Error.Detail)
	}
	if step.Error.Public == nil || step.Error.Public.Code != "lookup_failed" {
		t.Errorf("public: %+v", step.Error.Public)
	}
}

func TestStreamReasoning_FailureErrorTexts(t *testing.T) {
	cause := errors.New("vendor X stream: connection reset")
	streamErr := Fail(cause, "the response stream was interrupted", PublicFacing{Code: "stream_down", Message: "回复中断"})

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		_ = w.StreamReasoning(ctx, "think", func(ReasoningStream) error { return streamErr })
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: TextUserMessage("x")})

	reasoning := turn.Items[0]
	if reasoning.Kind != ItemKindReasoning || reasoning.Status != ItemStatusFailed || reasoning.Error == nil {
		t.Fatalf("reasoning item: %+v", reasoning)
	}
	if reasoning.Error.Code != "stream_failed" {
		t.Errorf("code = %q, want stream_failed", reasoning.Error.Code)
	}
	if reasoning.Error.Message != "the response stream was interrupted" {
		t.Errorf("message = %q, want the model-facing text", reasoning.Error.Message)
	}
	if reasoning.Error.Detail != "vendor X stream: connection reset" {
		t.Errorf("detail = %q, want the raw cause text", reasoning.Error.Detail)
	}
	if reasoning.Error.Public == nil || reasoning.Error.Public.Code != "stream_down" {
		t.Errorf("public: %+v", reasoning.Error.Public)
	}
}

// ===== Serialization =====

func TestItemError_JSONExcludesDetail(t *testing.T) {
	t.Parallel()
	itemErr := &ItemError{
		Code:    "tool_failed",
		Message: "the search tool is temporarily unavailable",
		Detail:  "vendor X upstream: 500 quota exceeded",
		Public:  &PublicFacing{Code: "search_down", Message: "搜索服务暂时不可用"},
	}

	for name, marshal := range map[string]func(any) ([]byte, error){
		"jsonv2": func(v any) ([]byte, error) { return jsonv2.Marshal(v) },
		"std":    stdjson.Marshal,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := marshal(itemErr)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if bytes.Contains(data, []byte("vendor X upstream: 500 quota exceeded")) {
				t.Errorf("payload %s leaks the raw detail", data)
			}
			if bytes.Contains(data, []byte("Detail")) {
				t.Errorf("payload %s carries a Detail key", data)
			}
			var fields map[string]any
			if err := stdjson.Unmarshal(data, &fields); err != nil {
				t.Fatalf("unmarshal back: %v", err)
			}
			for _, key := range []string{"Code", "Message", "Public"} {
				if _, ok := fields[key]; !ok {
					t.Errorf("payload %s lost key %q", data, key)
				}
			}
			public, _ := fields["Public"].(map[string]any)
			if public == nil || public["Code"] != "search_down" {
				t.Errorf("payload %s lost the public block", data)
			}
		})
	}

	// A nil Public omits the field entirely rather than serializing null.
	plain, err := stdjson.Marshal(&ItemError{Code: "tool_failed", Message: "boom", Detail: "boom"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(plain, []byte("Public")) || bytes.Contains(plain, []byte("Detail")) {
		t.Errorf("payload %s must omit Public and Detail", plain)
	}
}
