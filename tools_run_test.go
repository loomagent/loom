package loom

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type echoToolArguments struct {
	Value string `json:"value"`
}

type queryToolArguments struct {
	Query string `json:"query"`
}

func newEchoTool() Tool {
	return NewTool(MustToolContract[echoToolArguments]("echo"), "echo back",
		func(_ context.Context, args echoToolArguments) (string, error) {
			return `{"got":"` + args.Value + `"}`, nil
		},
	)
}

func newFailingTool() Tool {
	return NewTool(MustToolContract[NoArguments]("bad"), "always fail",
		func(context.Context, NoArguments) (string, error) {
			return "", errors.New("boom")
		},
	)
}

// ===== ExecuteToolCalls(ReAct 场景) =====

func TestExecuteToolCalls_Success(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(newEchoTool())

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		// 模拟 LLM 返了两个 tool_calls
		calls := []ToolCall{
			{ID: "llm_c1", Name: "echo", Arguments: `{"value":"a"}`},
			{ID: "llm_c2", Name: "echo", Arguments: `{"value":"b"}`},
		}
		results, err := ExecuteToolCalls(ctx, w, reg, calls)
		if err != nil {
			return err
		}
		if len(results) != 2 {
			t.Fatalf("results len = %d", len(results))
		}
		if results[0].Output != `{"got":"a"}` || results[1].Output != `{"got":"b"}` {
			t.Errorf("outputs: %+v", results)
		}
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	if turn.Status != TurnStatusCompleted {
		t.Fatalf("status: %v", turn.Status)
	}
	// 期望:tool_call(c1) / tool_result(c1) / tool_call(c2) / tool_result(c2) / final_answer
	if len(turn.Items) != 5 {
		t.Fatalf("items: %d", len(turn.Items))
	}
	if turn.Items[0].ToolCallID != "llm_c1" {
		t.Errorf("first tool_call ID: %s", turn.Items[0].ToolCallID)
	}
	if turn.Items[2].ToolCallID != "llm_c2" {
		t.Errorf("second tool_call ID: %s", turn.Items[2].ToolCallID)
	}
}

func TestExecuteToolCalls_ToolFails(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(newFailingTool())

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		calls := []ToolCall{{ID: "c1", Name: "bad", Arguments: "{}"}}
		results, _ := ExecuteToolCalls(ctx, w, reg, calls)
		if results[0].Err == nil {
			t.Error("expected err")
		}
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	tr := turn.Items[1]
	if tr.Status != ItemStatusFailed || tr.Error == nil || tr.Error.Code != "tool_failed" {
		t.Errorf("tool_result: %+v", tr)
	}
}

// ===== RunToolByName(代码编排场景) =====

func TestRunToolByName_Success(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(NewTool(MustToolContract[queryToolArguments]("query"), "q",
		func(_ context.Context, args queryToolArguments) (string, error) {
			data, err := json.Marshal(args)
			return string(data), err
		},
	))

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		output, err := RunToolByName(ctx, w, "查询 1", reg, "query", queryToolArguments{Query: "ai"})
		if err != nil {
			return err
		}
		if output != `{"query":"ai"}` {
			t.Errorf("output: %s", output)
		}
		// 再调一次,验证 callID 递增
		output2, _ := RunToolByName(ctx, w, "查询 2", reg, "query", queryToolArguments{Query: "ml"})
		if output2 != `{"query":"ml"}` {
			t.Errorf("output2: %s", output2)
		}
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	// turn[0].tool_call[0] callID 应为 "call_0";第二次 "call_1"
	if turn.Items[0].ToolCallID != "call_0" {
		t.Errorf("first callID: %s", turn.Items[0].ToolCallID)
	}
	if turn.Items[2].ToolCallID != "call_1" {
		t.Errorf("second callID: %s", turn.Items[2].ToolCallID)
	}
}

func TestRunToolByName_NotFound(t *testing.T) {
	reg := NewToolRegistry()

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		_, err := RunToolByName(ctx, w, "x", reg, "missing", map[string]string{})
		if err == nil {
			t.Error("expected err")
		}
		return w.FinalAnswer(ctx, "ok")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	tr := turn.Items[1]
	if tr.Error == nil || tr.Error.Code != "tool_not_found" {
		t.Errorf("tool_result.Error: %+v", tr.Error)
	}
}
