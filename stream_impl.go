package loom

import (
	"context"
	"strings"
	"sync"
)

// streamCore 流式 Item 共用的累积态。
type streamCore struct {
	mu        sync.Mutex
	accumText strings.Builder
	// finalText 业务方 SetFinalText 设置;nil 表示用 accumText 累积值。
	finalText *string
}

// appendLocked mu 已持有时调。
func (c *streamCore) appendLocked(chunk string) {
	c.accumText.WriteString(chunk)
}

// finalize 取最终文本(SetFinalText 给的或累积值)。
func (c *streamCore) finalize() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finalText != nil {
		return *c.finalText
	}
	return c.accumText.String()
}

// itemTextStream 同时实现 ReasoningStream 和 FinalAnswerStream:两个接口的方法集
// 相同,所以一个类型满足两者,不必各留一份一模一样的实现。
type itemTextStream struct {
	core     streamCore
	state    *turnState
	itemPath string
}

func (s *itemTextStream) AppendText(ctx context.Context, chunk string) error {
	if chunk == "" {
		return nil
	}
	s.core.mu.Lock()
	s.core.appendLocked(chunk)
	s.core.mu.Unlock()
	s.state.emitItemDelta(ctx, s.itemPath, DeltaChannelText, chunk)
	return nil
}

func (s *itemTextStream) SetFinalText(text string) {
	s.core.mu.Lock()
	s.core.finalText = &text
	s.core.mu.Unlock()
}
