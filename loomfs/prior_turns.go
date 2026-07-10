package loomfs

import (
	"context"
	"fmt"
)

func LoadPriorTurns(root string) ([]PriorTurn, error) {
	ws, err := OpenWorkspace(root)
	if err != nil {
		return nil, err
	}
	snapshot, err := ws.LoadSnapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		return nil, err
	}
	return snapshot.PriorTurns, nil
}

func AppendPriorTurn(root string, turn PriorTurn) error {
	ws, err := OpenWorkspace(root)
	if err != nil {
		return err
	}
	if turn.TurnIndex == 0 {
		return fmt.Errorf("prior_turns: turn_index 必填")
	}
	if turn.At.IsZero() {
		turn.At = now()
	}
	return ws.appendEvents([]JournalEvent{{Type: eventTypeTurnCompleted, PriorTurn: &turn, At: now()}})
}
