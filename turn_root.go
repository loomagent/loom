package loom

import (
	"context"
	"fmt"
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

// FinalAnswer writes the final answer, Completed at once. It commits once per Turn: a later call
// returns ErrTurnClosed, and a call made while another commit is streaming returns
// ErrFinalAnswerInProgress.
func (t *turnRoot) FinalAnswer(ctx context.Context, text string) error {
	return t.writeFinalAnswer(ctx, text, false, nil)
}

// StreamFinalAnswer streams the final answer. A nil return seals the Turn; an error marks this
// attempt as a candidate that failed, releases the commit, and returns, so product code may retry
// or return. Only a committed answer reaches the LLM history.
func (t *turnRoot) StreamFinalAnswer(ctx context.Context, fn func(FinalAnswerStream) error) error {
	return t.writeFinalAnswer(ctx, "", true, fn)
}

// writeFinalAnswer is the shared entry point for the one-shot and streaming paths.
//
// The commit is claimed and applied under one lock, which is what makes "one final answer" a
// guarantee rather than a hope:
//   - streaming=false: the item is Completed and the Turn is sealed in the same critical
//     section, and Started and Finished are emitted afterwards
//   - streaming=true: the item starts InProgress and the claim is held while the closure runs;
//     the closure's result then decides, again under the lock, between committing and releasing
//     the claim so a retry stays possible
func (t *turnRoot) writeFinalAnswer(
	ctx context.Context,
	text string,
	streaming bool,
	fn func(FinalAnswerStream) error,
) error {
	// 1. claim the commit and allocate the final_answer item
	t.state.mu.Lock()
	if t.state.isClosed() {
		t.state.mu.Unlock()
		return ErrTurnClosed
	}
	if t.state.finalAnswerInProgress {
		t.state.mu.Unlock()
		return ErrFinalAnswerInProgress
	}
	t.state.finalAnswerInProgress = true
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
		// The answer and the seal land together, so no reader can observe a sealed Turn whose
		// answer has not been appended yet.
		t.state.sealFinalAnswerLocked()
	}
	children := t.locateChildrenLocked()
	*children = append(*children, item)
	selfIdx := len(*children) - 1
	t.state.mu.Unlock()

	t.state.emitItemStarted(ctx, item)

	if !streaming {
		t.state.emitItemFinished(ctx, item)
		return nil
	}

	// Streaming: run the closure, then commit or release under the lock. A panic is a failure
	// like any other: it must not leave the commit claimed and the item in progress, so the state
	// is settled before the panic continues to the caller.
	stream := &itemTextStream{state: t.state, itemPath: path}
	fnErr := t.runStreamingFinalAnswer(ctx, selfIdx, stream, fn)
	snapshot := t.finalizeStreamItem(selfIdx, stream.core.finalize(), fnErr)

	t.state.mu.Lock()
	switch {
	case t.state.isClosed():
		// An external cancel or a dispatcher failure closed the Turn while the closure ran, so
		// this attempt is not the reason it closed; the claim is simply abandoned.
		t.state.finalAnswerInProgress = false
	case fnErr == nil:
		t.state.sealFinalAnswerLocked()
	default:
		// A failed candidate is not a commit: releasing the claim is what keeps a retry possible.
		t.state.finalAnswerInProgress = false
	}
	t.state.mu.Unlock()
	t.state.emitItemFinished(ctx, snapshot)
	return fnErr
}

// runStreamingFinalAnswer runs the caller's closure and keeps the commit consistent if it panics.
func (t *turnRoot) runStreamingFinalAnswer(
	ctx context.Context,
	selfIdx int,
	stream *itemTextStream,
	fn func(FinalAnswerStream) error,
) (err error) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		panicErr := fmt.Errorf("loom: final answer stream panicked: %v", recovered)
		snapshot := t.finalizeStreamItem(selfIdx, stream.core.finalize(), panicErr)
		t.state.mu.Lock()
		t.state.finalAnswerInProgress = false
		t.state.mu.Unlock()
		t.state.emitItemFinished(ctx, snapshot)
		panic(recovered)
	}()
	return fn(stream)
}
