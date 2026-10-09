package loom

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
)

func Test_user_sees_specific_failure_reason_through_wrapped_errors(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		cause error
		code  CloseCode
	}{
		{"provider content rejection", ErrSensitiveContentRisk, CloseCodeContentFilter},
		{"output moderation", ErrContentFilter, CloseCodeContentFilter},
		{"truncated output", ErrOutputTruncated, CloseCodeOutputTruncated},
		{"ordinary error", errors.New("invalid messages"), CloseCodeAgentError},
	} {
		t.Run("user "+scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given a provider-neutral failure wrapped by the execution layers.
				cause := fmt.Errorf("react step: %w", fmt.Errorf("model call: %w", scenario.cause))
				// When the user's turn fails without a committed final answer.
				turn, err := Run(t.Context(), func(context.Context, TurnWriter, []Turn, UserMessage) error {
					return cause
				}, RunOptions{ConversationID: "fixture", Input: UserMessage{Text: "user task"}})
				// Then the specific close code and original cause survive to the caller.
				if turn == nil || turn.Status != TurnStatusFailed || turn.CloseReason == nil || turn.CloseReason.Code != scenario.code {
					t.Fatalf("turn=%+v want failed/%s", turn, scenario.code)
				}
				if !errors.Is(err, scenario.cause) || !errors.Is(turn.CloseReason.Cause, scenario.cause) {
					t.Fatal("original classified cause was lost")
				}
			})
		})
	}
}
