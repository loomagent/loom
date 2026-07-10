package loom

import (
	"context"
	"errors"
	"testing"
)

func TestRun_Completed(t *testing.T) {
	sink := NewMemorySink()
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, history []Turn, input UserMessage) error {
		return w.FinalAnswer(ctx, "answer: "+input.Text)
	}, RunOptions{
		ConversationID: "conv_test",
		Sinks:          []Sink{sink},
		Input:          UserMessage{Text: "hello"},
	})

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	if turn.Status != TurnStatusCompleted {
		t.Errorf("status = %v, want completed", turn.Status)
	}
	if turn.CloseReason == nil || turn.CloseReason.Code != CloseCodeFinalAnswer {
		t.Errorf("close reason = %+v", turn.CloseReason)
	}
	// loom.Run 不再自动写 user_message → 只有 final_answer
	if len(turn.Items) != 1 {
		t.Fatalf("items count = %d, want 1", len(turn.Items))
	}
	if turn.Items[0].Kind != ItemKindFinalAnswer || turn.Items[0].Text != "answer: hello" {
		t.Errorf("first item: %+v", turn.Items[0])
	}
	if turn.Items[0].Path != "turn[0].final_answer" {
		t.Errorf("final_answer path: %q", turn.Items[0].Path)
	}
	// Sink:final_answer 一对 Started/Finished = 2 帧
	if got := len(sink.StartedEvents()); got != 1 {
		t.Errorf("started events = %d, want 1", got)
	}
	if got := len(sink.FinishedEvents()); got != 1 {
		t.Errorf("finished events = %d, want 1", got)
	}
}

func TestRun_AgentError(t *testing.T) {
	sink := NewMemorySink()
	sentinel := errors.New("boom")
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, history []Turn, input UserMessage) error {
		return sentinel
	}, RunOptions{
		ConversationID: "conv_test",
		Sinks:          []Sink{sink},
		Input:          UserMessage{Text: "x"},
	})

	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	if turn.Status != TurnStatusFailed {
		t.Errorf("status = %v, want failed", turn.Status)
	}
	if turn.CloseReason == nil || turn.CloseReason.Code != CloseCodeAgentError {
		t.Errorf("close reason = %+v", turn.CloseReason)
	}
}

func TestRun_NoFinalAnswer(t *testing.T) {
	sink := NewMemorySink()
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, history []Turn, input UserMessage) error {
		return nil // 没调 FinalAnswer
	}, RunOptions{
		ConversationID: "conv_test",
		Sinks:          []Sink{sink},
		Input:          UserMessage{Text: "x"},
	})

	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}
	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	if turn.Status != TurnStatusFailed {
		t.Errorf("status = %v, want failed", turn.Status)
	}
	if turn.CloseReason == nil || turn.CloseReason.Code != CloseCodeNoFinal {
		t.Errorf("close reason = %+v", turn.CloseReason)
	}
}

func TestRun_Cancelled(t *testing.T) {
	sink := NewMemorySink()
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, history []Turn, input UserMessage) error {
		return context.Canceled
	}, RunOptions{
		ConversationID: "conv_test",
		Sinks:          []Sink{sink},
		Input:          UserMessage{Text: "x"},
	})

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	if turn.Status != TurnStatusCancelled {
		t.Errorf("status = %v, want cancelled", turn.Status)
	}
	if turn.CloseReason == nil || turn.CloseReason.Code != CloseCodeUserCancel {
		t.Errorf("close reason = %+v", turn.CloseReason)
	}
}

func TestRun_CancelledStepItem(t *testing.T) {
	sink := NewMemorySink()
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, history []Turn, input UserMessage) error {
		return w.Step(ctx, "running", func(ctx context.Context, s Step) error {
			return context.Canceled
		})
	}, RunOptions{
		ConversationID: "conv_test",
		Sinks:          []Sink{sink},
		Input:          UserMessage{Text: "x"},
	})

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	if turn.Status != TurnStatusCancelled {
		t.Errorf("status = %v, want cancelled", turn.Status)
	}
	if len(turn.Items) != 1 {
		t.Fatalf("items count = %d, want 1", len(turn.Items))
	}
	if turn.Items[0].Status != ItemStatusCancelled || turn.Items[0].Error != nil {
		t.Errorf("step item = %+v, want cancelled without error", turn.Items[0])
	}
}

func TestRun_ContextCancelCauseHostShutdown(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	turn, err := Run(ctx, func(ctx context.Context, w TurnWriter, history []Turn, input UserMessage) error {
		cancel(ErrHostShutdown)
		return ctx.Err()
	}, RunOptions{
		ConversationID: "conv_test",
		Input:          UserMessage{Text: "x"},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if turn.CloseReason == nil || turn.CloseReason.Code != CloseCodeHostShutdown {
		t.Fatalf("close reason = %+v, want host_shutdown", turn.CloseReason)
	}
}

func TestRun_ContextCancelCauseExternalCancel(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	turn, err := Run(ctx, func(ctx context.Context, w TurnWriter, history []Turn, input UserMessage) error {
		cancel(ErrExternalCancel)
		return ctx.Err()
	}, RunOptions{
		ConversationID: "conv_test",
		Input:          UserMessage{Text: "x"},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if turn.CloseReason == nil || turn.CloseReason.Code != CloseCodeExternalCancel {
		t.Fatalf("close reason = %+v, want external_cancel", turn.CloseReason)
	}
}

func TestRun_StepNesting(t *testing.T) {
	sink := NewMemorySink()
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, history []Turn, input UserMessage) error {
		err := w.Step(ctx, "调研", func(ctx context.Context, s Step) error {
			return s.Step(ctx, "第 1 轮", func(ctx context.Context, round Step) error {
				return round.WriteReasoning(ctx, "规划", "...")
			})
		})
		if err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "done")
	}, RunOptions{
		ConversationID: "conv_test",
		Sinks:          []Sink{sink},
		Input:          UserMessage{Text: "x"},
	})

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	if turn.Status != TurnStatusCompleted {
		t.Errorf("status = %v", turn.Status)
	}
	// 根 items:step[0]("调研") / final_answer
	if len(turn.Items) != 2 {
		t.Fatalf("root items = %d, want 2", len(turn.Items))
	}
	stage := turn.Items[0]
	if stage.Kind != ItemKindStep || stage.Label != "调研" {
		t.Errorf("stage: %+v", stage)
	}
	if stage.Path != "turn[0].step[0]" {
		t.Errorf("stage path: %q", stage.Path)
	}
	if len(stage.Children) != 1 || stage.Children[0].Label != "第 1 轮" {
		t.Errorf("round: %+v", stage.Children)
	}
	round := stage.Children[0]
	if round.Path != "turn[0].step[0].step[0]" {
		t.Errorf("round path: %q", round.Path)
	}
	if len(round.Children) != 1 || round.Children[0].Kind != ItemKindReasoning {
		t.Errorf("round children: %+v", round.Children)
	}
	if round.Children[0].Path != "turn[0].step[0].step[0].reasoning[0]" {
		t.Errorf("reasoning path: %q", round.Children[0].Path)
	}
}
