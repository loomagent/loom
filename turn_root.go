package loom

import (
	"context"
	"time"
)

// turnRoot implements TurnWriter. Every Writer method comes from writerScope; only
// FinalAnswer and StreamFinalAnswer belong to the turn root.
type turnRoot struct {
	*writerScope
}

// Compile-time interface assertions.
var _ TurnWriter = (*turnRoot)(nil)

// newTurnRoot builds the Turn root Writer that Run hands to a handler.
func newTurnRoot(state *turnState) *turnRoot {
	return &turnRoot{writerScope: newRootScope(state)}
}

// FinalAnswer writes the final answer, Completed at once. It may be called once
// per Turn; the second call returns ErrTurnClosed.
func (t *turnRoot) FinalAnswer(ctx context.Context, text string) error {
	return t.writeFinalAnswer(ctx, text, false, nil)
}

// StreamFinalAnswer streams the final answer. A nil return seals the Turn; an
// error leaves it unsealed, so product code may retry or return.
func (t *turnRoot) StreamFinalAnswer(ctx context.Context, fn func(FinalAnswerStream) error) error {
	return t.writeFinalAnswer(ctx, "", true, fn)
}

// writeFinalAnswer is the shared entry point for the one-shot and streaming paths.
//   - streaming=false: the item is Completed at once, and Started and Finished are
//     emitted after sealing
//   - streaming=true: the item starts InProgress, the closure runs, the item is
//     finalized from its return value, and the Turn seals only on success
func (t *turnRoot) writeFinalAnswer(
	ctx context.Context,
	text string,
	streaming bool,
	fn func(FinalAnswerStream) error,
) error {
	// 1. check the seal and allocate the final_answer item
	t.state.mu.Lock()
	if t.state.isClosed() {
		t.state.mu.Unlock()
		return ErrTurnClosed
	}
	idx := t.counters[ItemKindFinalAnswer]
	t.counters[ItemKindFinalAnswer] = idx + 1
	path := formatChildPath(t.path, ItemKindFinalAnswer, idx)
	now := time.Now()
	item := Item{
		Kind:      ItemKindFinalAnswer,
		Index:     idx,
		Path:      path,
		Status:    ItemStatusInProgress, // streaming starts here; the one-shot path overwrites it below
		StartedAt: now,
		UpdatedAt: now,
	}
	if !streaming {
		item.Text = text
		item.Status = ItemStatusCompleted
	}
	children := t.locateChildrenLocked()
	*children = append(*children, item)
	selfIdx := len(*children) - 1
	t.state.mu.Unlock()

	t.state.emitItemStarted(ctx, item)

	if !streaming {
		// One-shot: seal, then emit Finished
		t.seal(ctx)
		t.state.emitItemFinished(ctx, item)
		return nil
	}

	// Streaming: run the closure, finalize, and seal only on success
	stream := &itemTextStream{state: t.state, itemPath: path}
	fnErr := fn(stream)
	snapshot := t.finalizeStreamItem(selfIdx, stream.core.finalize(), fnErr)
	if fnErr == nil {
		t.seal(ctx)
	}
	t.state.emitItemFinished(ctx, snapshot)
	return fnErr
}

// seal sets state.closeReason to {Completed, "final_answer"}. The caller holds no
// lock; this method takes it.
func (t *turnRoot) seal(_ context.Context) {
	t.state.mu.Lock()
	defer t.state.mu.Unlock()
	t.state.closeReason = &CloseReason{
		Code: CloseCodeFinalAnswer,
	}
}
