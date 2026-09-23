package loom

import (
	"context"
	"testing"
)

func TestUserSeesPolicyFailureWithoutSuccessfulTurn(t *testing.T) {
	// Given a host policy rejection, a visible reply must still be a failed turn.
	turn, err := Run(t.Context(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		return w.Fail(ctx, "Cannot complete this request.", CloseCode("host_policy_rejected"))
	}, RunOptions{ConversationID: "failure", Input: UserMessage{Text: "request"}})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != TurnStatusFailed || turn.CloseReason.Code != "host_policy_rejected" {
		t.Fatalf("outcome=%+v", turn)
	}
	if len(turn.Items) != 1 || turn.Items[0].Text != "Cannot complete this request." {
		t.Fatalf("reply=%+v", turn.Items)
	}
}
