package loom

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type cancellingSink struct {
	*MemorySink
	cancel  context.CancelFunc
	failure error
}

func (s *cancellingSink) ItemDelta(ctx context.Context, e ItemDeltaEvent) error {
	s.cancel()
	if s.failure != nil {
		return s.failure
	}
	return fmt.Errorf("persistence cancelled: %w", ctx.Err())
}
func TestUserCancellationDuringPersistenceKeepsCancellationStatus(t *testing.T) {
	for _, independentFailure := range []bool{false, true} {
		name := "user_cancels_during_database_write"
		if independentFailure {
			name = "user_still_sees_independent_storage_failure"
		}
		t.Run(name, func(t *testing.T) {
			// Given a stream whose persistence races with the user's cancellation.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sink := &cancellingSink{MemorySink: NewMemorySink(), cancel: cancel}
			if independentFailure {
				sink.failure = errors.New("storage unavailable")
			}
			// When a streamed chunk reaches that sink.
			turn, err := Run(ctx, func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
				return w.StreamReasoning(ctx, "", func(s ReasoningStream) error { return s.AppendText(ctx, "partial") })
			}, RunOptions{ConversationID: "cancel", StrictSink: true, Sinks: []Sink{sink}})
			// Then cancellation is distinct from an independent persistence error.
			if independentFailure {
				if turn.Status != TurnStatusFailed || !errors.Is(turn.CloseReason.Cause, sink.failure) {
					t.Fatalf("turn=%+v err=%v", turn, err)
				}
			} else if turn.Status != TurnStatusCancelled || turn.CloseReason.Code != CloseCodeUserCancel || err != nil {
				t.Fatalf("turn=%+v err=%v", turn, err)
			}
		})
	}
}

func TestUserCancellationDuringFinalPersistenceCannotComplete(t *testing.T) {
	// Given a final stream interrupted while its chunk is persisted.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink := &cancellingSink{MemorySink: NewMemorySink(), cancel: cancel}
	// When final delivery encounters the cancelled write, it cannot commit success.
	turn, err := Run(ctx, func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		return w.StreamFinalAnswer(ctx, func(s FinalAnswerStream) error { return s.AppendText(ctx, "partial") })
	}, RunOptions{ConversationID: "cancel-final", StrictSink: true, Sinks: []Sink{sink}})
	// Then the user sees cancellation, not successful or failed generation.
	if turn.Status != TurnStatusCancelled || turn.CloseReason.Code != CloseCodeUserCancel || err != nil {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
}
