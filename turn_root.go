package loom

import (
	"context"
	"time"
)

// turnRoot 实现 TurnWriter 接口。
// Writer 方法全部继承 writerScope,仅 FinalAnswer / StreamFinalAnswer 为 turn 根专属。
type turnRoot struct {
	*writerScope
}

// 编译期接口断言。
var _ TurnWriter = (*turnRoot)(nil)

// newTurnRoot 构造 Turn 根 Writer(供 Run 使用)。
func newTurnRoot(state *turnState) *turnRoot {
	return &turnRoot{writerScope: newRootScope(state)}
}

// FinalAnswer 写最终回答(立即 Completed)。
// 每个 Turn 只能调一次,第二次返 ErrTurnClosed。
func (t *turnRoot) FinalAnswer(ctx context.Context, text string) error {
	return t.writeFinalAnswer(ctx, text, false, nil)
}

// StreamFinalAnswer 流式写最终回答。
// 闭包返 nil → Turn 封口;返 error → 不封口(业务方可重试或直接 return)。
func (t *turnRoot) StreamFinalAnswer(ctx context.Context, fn func(FinalAnswerStream) error) error {
	return t.writeFinalAnswer(ctx, "", true, fn)
}

// writeFinalAnswer 一次性 / 流式共用入口。
//   - streaming=false:item 立即 Completed,sealed 后 emit Started+Finished
//   - streaming=true:item 先 InProgress,跑闭包,根据返回值 finalize,成功时才 sealed
func (t *turnRoot) writeFinalAnswer(
	ctx context.Context,
	text string,
	streaming bool,
	fn func(FinalAnswerStream) error,
) error {
	// 1. check sealed + 分配 final_answer item
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
		Status:    ItemStatusInProgress, // 流式先 InProgress;一次性下面立即覆盖
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
		// 一次性:封口 + emit Finished
		t.seal(ctx)
		t.state.emitItemFinished(ctx, item)
		return nil
	}

	// 流式:跑闭包 → finalize → 成功才封口
	stream := &finalAnswerStream{state: t.state, itemPath: path}
	fnErr := fn(stream)
	snapshot := t.finalizeStreamItem(selfIdx, stream.core.finalize(), fnErr)
	if fnErr == nil {
		t.seal(ctx)
	}
	t.state.emitItemFinished(ctx, snapshot)
	return fnErr
}

// seal 翻 state.closeReason = {Completed, "final_answer"}。
// 调用方不持 mu,本方法内部加锁。
func (t *turnRoot) seal(_ context.Context) {
	t.state.mu.Lock()
	defer t.state.mu.Unlock()
	t.state.closeReason = &CloseReason{
		Code: CloseCodeFinalAnswer,
	}
}
