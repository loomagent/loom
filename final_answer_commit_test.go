package loom

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// finalAnswerItems returns every final_answer item in the Turn, in tree order.
func finalAnswerItems(turn *Turn) []Item {
	var out []Item
	walkItems(turn.Items, func(item *Item) {
		if item.Kind == ItemKindFinalAnswer {
			out = append(out, *item)
		}
	})
	return out
}

// Two commits cannot both win: the answer and the seal land in one critical section, so the loser
// finds the Turn closed instead of appending a second answer.
func TestFinalAnswerCommitsOnce(t *testing.T) {
	var turn *Turn
	var err error
	turn, err = Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		var wg sync.WaitGroup
		results := make([]error, 2)
		for i := range results {
			wg.Go(func() {
				results[i] = w.FinalAnswer(ctx, "answer")
			})
		}
		wg.Wait()
		accepted := 0
		for _, result := range results {
			if result == nil {
				accepted++
				continue
			}
			if !errors.Is(result, ErrTurnClosed) {
				t.Errorf("loser error = %v, want ErrTurnClosed", result)
			}
		}
		if accepted != 1 {
			t.Errorf("commits accepted = %d, want exactly 1", accepted)
		}
		return nil
	}, RunOptions{ConversationID: "commit-once"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if items := finalAnswerItems(turn); len(items) != 1 {
		t.Fatalf("final_answer items = %d, want 1", len(items))
	}
	if turn.Status != TurnStatusCompleted {
		t.Fatalf("status = %s", turn.Status)
	}
}

// A commit made while another one is streaming is refused with its own error, because the Turn is
// still open and a retry may still be possible.
func TestFinalAnswerRefusedWhileAnotherCommits(t *testing.T) {
	release := make(chan struct{})
	streaming := make(chan struct{})
	var turn *Turn
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		streamErr := make(chan error, 1)
		go func() {
			streamErr <- w.StreamFinalAnswer(ctx, func(stream FinalAnswerStream) error {
				if err := stream.AppendText(ctx, "the answer"); err != nil {
					return err
				}
				close(streaming)
				<-release
				return nil
			})
		}()
		<-streaming
		if err := w.FinalAnswer(ctx, "another answer"); !errors.Is(err, ErrFinalAnswerInProgress) {
			t.Errorf("second commit error = %v, want ErrFinalAnswerInProgress", err)
		}
		close(release)
		return <-streamErr
	}, RunOptions{ConversationID: "commit-in-progress"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	items := finalAnswerItems(turn)
	if len(items) != 1 || items[0].Text != "the answer" || items[0].Status != ItemStatusCompleted {
		t.Fatalf("items = %+v", items)
	}
}

// A failed stream is a candidate, not a commit: the next attempt may commit, the failed attempt
// stays as an audit record, and only the committed answer reaches the LLM history.
func TestFailedFinalAnswerReleasesTheCommitAndStaysOutOfHistory(t *testing.T) {
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		if err := w.StreamFinalAnswer(ctx, func(stream FinalAnswerStream) error {
			if err := stream.AppendText(ctx, "a draft"); err != nil {
				return err
			}
			return errors.New("the draft was rejected")
		}); err == nil {
			t.Error("a failing closure must return its error")
		}
		return w.FinalAnswer(ctx, "the answer")
	}, RunOptions{ConversationID: "commit-retry"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	items := finalAnswerItems(turn)
	if len(items) != 2 {
		t.Fatalf("final_answer items = %d, want 2 (one failed candidate, one commit)", len(items))
	}
	// A store that keys rows by path — as the reference sink does — cannot tell two attempts
	// apart unless every attempt has its own path.
	if items[0].Path != "turn[0].final_answer[0]" || items[1].Path != "turn[0].final_answer[1]" {
		t.Fatalf("attempt paths = %q, %q, want turn[0].final_answer[0] and [1]", items[0].Path, items[1].Path)
	}
	if items[0].Status != ItemStatusFailed || !strings.Contains(items[0].Error.Message, "the draft was rejected") {
		t.Fatalf("candidate = %+v", items[0])
	}
	if items[1].Status != ItemStatusCompleted || items[1].Text != "the answer" {
		t.Fatalf("commit = %+v", items[1])
	}
	history, err := HistoryToMessages([]Turn{*turn}, UserMessage{Text: "next"})
	if err != nil {
		t.Fatalf("HistoryToMessages: %v", err)
	}
	for _, message := range history {
		if strings.Contains(message.Content, "a draft") {
			t.Fatalf("a rejected candidate reached the history: %+v", history)
		}
	}
	if history[len(history)-2].Content != "the answer" {
		t.Fatalf("history = %+v", history)
	}
}

// A panic in the streaming closure is a failure like any other: it must not leave the commit
// claimed, so the handler can recover and still commit.
func TestPanickingFinalAnswerReleasesTheCommit(t *testing.T) {
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic must reach the caller")
				}
			}()
			_ = w.StreamFinalAnswer(ctx, func(FinalAnswerStream) error {
				panic("boom")
			})
		}()
		return w.FinalAnswer(ctx, "the answer")
	}, RunOptions{ConversationID: "commit-panic"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	items := finalAnswerItems(turn)
	if len(items) != 2 {
		t.Fatalf("final_answer items = %d, want 2", len(items))
	}
	if items[0].Status != ItemStatusFailed || !strings.Contains(items[0].Error.Message, "boom") {
		t.Fatalf("panicked attempt = %+v", items[0])
	}
	if items[1].Status != ItemStatusCompleted {
		t.Fatalf("commit after the panic = %+v", items[1])
	}
}
