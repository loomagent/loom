package loomfs

import "context"

func LoadTurnContexts(root string) ([]TurnContext, error) {
	ws, err := OpenWorkspace(root)
	if err != nil {
		return nil, err
	}
	snapshot, err := ws.LoadSnapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		return nil, err
	}
	return snapshot.TurnContexts, nil
}
