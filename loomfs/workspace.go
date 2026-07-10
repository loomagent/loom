package loomfs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Workspace struct {
	root string
	mu   sync.Mutex
}

func OpenWorkspace(root string) (*Workspace, error) {
	root, err := cleanRoot(root)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{rawDirName, contextDirName} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, fmt.Errorf("loomfs: create %s dir: %w", dir, err)
		}
	}
	return &Workspace{root: root}, nil
}

func (w *Workspace) Root() string { return w.root }

func (w *Workspace) BeginTurn(meta TurnMeta) (*TurnSession, error) {
	if w == nil {
		return nil, fmt.Errorf("loomfs: workspace 为空")
	}
	if meta.TurnIndex == 0 {
		return nil, fmt.Errorf("loomfs: turn_index 必须从 1 开始")
	}
	snapshot, err := w.LoadSnapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		return nil, err
	}
	return newTurnSession(w, meta, snapshot), nil
}

func (w *Workspace) LoadSnapshot(_ context.Context, _ SnapshotOptions) (ContextSnapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.loadSnapshotLocked()
}

func (w *Workspace) appendEvents(events []JournalEvent) error {
	if len(events) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(w.root, journalFilename), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("loomfs: open journal: %w", err)
	}
	enc := json.NewEncoder(f)
	for _, ev := range events {
		if ev.At.IsZero() {
			ev.At = time.Now()
		}
		if err := enc.Encode(ev); err != nil {
			_ = f.Close()
			return fmt.Errorf("loomfs: write journal: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("loomfs: close journal: %w", err)
	}
	if err := w.rebuildViewsLocked(); err != nil {
		return err
	}
	return nil
}

func (w *Workspace) loadSnapshotLocked() (ContextSnapshot, error) {
	events, err := w.loadJournalLocked()
	if err != nil {
		return ContextSnapshot{}, err
	}
	return materialize(events, w.root), nil
}

func (w *Workspace) rebuildViewsLocked() error {
	snapshot, err := w.loadSnapshotLocked()
	if err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(w.root, queriesFilename), queryRecords(snapshot.Queries)); err != nil {
		return fmt.Errorf("loomfs: write queries view: %w", err)
	}
	if err := writeJSONL(filepath.Join(w.root, sourcesFilename), sourceEntries(snapshot.Sources)); err != nil {
		return fmt.Errorf("loomfs: write sources view: %w", err)
	}
	if err := writeJSONL(filepath.Join(w.root, priorTurnsFilename), priorTurns(snapshot.PriorTurns)); err != nil {
		return fmt.Errorf("loomfs: write prior turns view: %w", err)
	}
	if err := writeJSONL(filepath.Join(w.root, contextDirName, turnContextsFile), turnContexts(snapshot.TurnContexts)); err != nil {
		return fmt.Errorf("loomfs: write turn contexts view: %w", err)
	}
	return nil
}

func (w *Workspace) loadJournalLocked() ([]JournalEvent, error) {
	f, err := os.Open(filepath.Join(w.root, journalFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("loomfs: open journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	events := make([]JournalEvent, 0, 128)
	if err := forEachJSONLLine(f, func(line []byte) error {
		var ev JournalEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("loomfs: parse journal: %w", err)
		}
		events = append(events, ev)
		return nil
	}); err != nil {
		return nil, err
	}
	return events, nil
}
