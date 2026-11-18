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
				UserFacing{Code: "search_down", Message: "搜索服务暂时不可用，请稍后再试"},
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
	inner := Fail(cause, "inner text", UserFacing{Code: "inner", Message: "内层"})
	outer := Fail(inner, "outer text", UserFacing{Code: "outer", Message: "外层"})

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
	if fe.Model != "outer text" || fe.User.Code != "outer" {
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
	if got := Fail(cause, "model text", UserFacing{Code: "c", Message: "m"}).Error(); got != "vendor X upstream: 500" {
		t.Errorf("Error with cause = %q, want the cause text", got)
	}
	if got := Fail(nil, "model text", UserFacing{Code: "c", Message: "m"}).Error(); got != "model text" {
		t.Errorf("Error without cause = %q, want the model text", got)
	}
}

func TestFailureItemError_EmptyTextsKeepRawError(t *testing.T) {
	t.Parallel()
	err := Fail(errors.New("raw detail"), "", UserFacing{})
	itemErr := failureItemError("tool_failed", err)
	if itemErr.Message != "raw detail" || itemErr.Detail != "raw detail" {
		t.Errorf("message/detail: %+v", itemErr)
	}
	if itemErr.User != nil {
		t.Errorf("zero UserFacing must not become a pointer field: %+v", itemErr.User)
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
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

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
	if itemErr.User == nil || itemErr.User.Code != "search_down" || itemErr.User.Message != "搜索服务暂时不可用，请稍后再试" {
		t.Errorf("user: %+v", itemErr.User)
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
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

	itemErr := turn.Items[1].Error
	if itemErr == nil {
		t.Fatalf("tool_result has no error")
	}
	if itemErr.Code != "tool_failed" || itemErr.Message != "boom" || itemErr.Detail != "boom" {
		t.Errorf("item error: %+v", itemErr)
	}
	if itemErr.User != nil {
		t.Errorf("plain error must not grow a user block: %+v", itemErr.User)
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

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		// the model omitted the required "value" argument
		results, err := ExecuteToolCalls(ctx, w, reg, []ToolCall{{ID: "c1", Name: "echo", Arguments: `{}`}})
		if err != nil {
			return err
		}
		if argErr, ok := errors.AsType[*ToolArgumentError](results[0].Err); !ok || argErr.Tool != "echo" {
			t.Errorf("invoke error = %v, want *ToolArgumentError for tool echo", results[0].Err)
		}
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

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
	if itemErr.User != nil {
		t.Errorf("argument rejection must not grow a user block: %+v", itemErr.User)
	}
}

// ===== Model-facing tool message =====

func TestAppendAssistantTurn_FailureErrorModelText(t *testing.T) {
	t.Parallel()
	cause := errors.New("vendor X upstream: 500 quota exceeded")
	results := []ToolExecResult{{
		Call: ToolCall{ID: "c1", Name: "search"},
		Err:  Fail(cause, "the search tool is temporarily unavailable", UserFacing{Code: "search_down", Message: "搜索暂时不可用"}),
	}}
	msgs := AppendAssistantTurn(nil, &ChatResponse{}, results)
	if len(msgs) != 2 || msgs[1].Role != RoleTool || msgs[1].ToolCallID != "c1" {
		t.Fatalf("msgs: %+v", msgs)
	}
	if !strings.Contains(msgs[1].Content, "the search tool is temporarily unavailable") {
		t.Errorf("content = %q, want it to carry the model-facing text", msgs[1].Content)
	}
	if strings.Contains(msgs[1].Content, "vendor X") {
		t.Errorf("content = %q, must not carry the raw cause text", msgs[1].Content)
	}
}

func TestAppendAssistantTurn_PlainErrorUnchanged(t *testing.T) {
	t.Parallel()
	results := []ToolExecResult{{Call: ToolCall{ID: "c1"}, Err: errors.New("boom")}}
	msgs := AppendAssistantTurn(nil, &ChatResponse{}, results)
	if msgs[1].Content != "tool execution error: boom" {
		t.Errorf("content = %q, want the raw error text as before", msgs[1].Content)
	}
}

// ===== Handler failure → CloseReason =====

func TestRun_HandlerFailureErrorFillsCloseReasonUser(t *testing.T) {
	cause := errors.New("worker panic: nil deref at vendor.go:42")
	handlerErr := Fail(fmt.Errorf("plan phase: %w", cause), "the task could not be finished",
		UserFacing{Code: "task_failed", Message: "任务暂时无法完成，请稍后重试"})

	turn, err := Run(context.Background(), func(context.Context, TurnWriter, []Turn, UserMessage) error {
		return handlerErr
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

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
	if cr.User == nil || cr.User.Code != "task_failed" || cr.User.Message != "任务暂时无法完成，请稍后重试" {
		t.Errorf("user: %+v", cr.User)
	}
	if !errors.Is(cr.Cause, cause) || !errors.Is(err, cause) {
		t.Error("the close reason and Run's error no longer reach the cause")
	}
}

func TestRun_PlainHandlerErrorHasNoUser(t *testing.T) {
	turn, _ := Run(context.Background(), func(context.Context, TurnWriter, []Turn, UserMessage) error {
		return errors.New("boom")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

	if cr := turn.CloseReason; cr == nil || cr.Message != "boom" || cr.User != nil {
		t.Errorf("close reason: %+v", cr)
	}
}

// ===== Step and streaming closures =====

func TestStep_FailureErrorTexts(t *testing.T) {
	cause := errors.New("vendor X upstream: refused")
	stepErr := Fail(cause, "the lookup step failed", UserFacing{Code: "lookup_failed", Message: "查询失败"})

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		_ = w.Step(ctx, "lookup", func(context.Context, Step) error { return stepErr })
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

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
	if step.Error.User == nil || step.Error.User.Code != "lookup_failed" {
		t.Errorf("user: %+v", step.Error.User)
	}
}

func TestStreamReasoning_FailureErrorTexts(t *testing.T) {
	cause := errors.New("vendor X stream: connection reset")
	streamErr := Fail(cause, "the response stream was interrupted", UserFacing{Code: "stream_down", Message: "回复中断"})

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		_ = w.StreamReasoning(ctx, "think", func(ReasoningStream) error { return streamErr })
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

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
	if reasoning.Error.User == nil || reasoning.Error.User.Code != "stream_down" {
		t.Errorf("user: %+v", reasoning.Error.User)
	}
}

// ===== Serialization =====

func TestItemError_JSONExcludesDetail(t *testing.T) {
	t.Parallel()
	itemErr := &ItemError{
		Code:    "tool_failed",
		Message: "the search tool is temporarily unavailable",
		Detail:  "vendor X upstream: 500 quota exceeded",
		User:    &UserFacing{Code: "search_down", Message: "搜索服务暂时不可用"},
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
			for _, key := range []string{"Code", "Message", "User"} {
				if _, ok := fields[key]; !ok {
					t.Errorf("payload %s lost key %q", data, key)
				}
			}
			user, _ := fields["User"].(map[string]any)
			if user == nil || user["Code"] != "search_down" {
				t.Errorf("payload %s lost the user block", data)
			}
		})
	}

	// A nil User omits the field entirely rather than serializing null.
	plain, err := stdjson.Marshal(&ItemError{Code: "tool_failed", Message: "boom", Detail: "boom"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(plain, []byte("User")) || bytes.Contains(plain, []byte("Detail")) {
		t.Errorf("payload %s must omit User and Detail", plain)
	}
}
