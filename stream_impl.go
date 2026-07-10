package loom

import (
	"context"
	"strings"
	"sync"
)

// streamCore 流式 Item 共用的累积态。
// 被 reasoningStream / finalAnswerStream 嵌入。
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

// reasoningStream 实现 ReasoningStream 接口。
type reasoningStream struct {
	core     streamCore
	state    *turnState
	itemPath string
}

func (s *reasoningStream) AppendText(ctx context.Context, chunk string) error {
	if chunk == "" {
		return nil
	}
	s.core.mu.Lock()
	s.core.appendLocked(chunk)
	s.core.mu.Unlock()
	s.state.emitItemDelta(ctx, s.itemPath, DeltaChannelText, chunk)
	return nil
}

func (s *reasoningStream) SetFinalText(text string) {
	s.core.mu.Lock()
	s.core.finalText = &text
	s.core.mu.Unlock()
}

// finalAnswerStream 实现 FinalAnswerStream 接口,跟 reasoningStream 结构一致。
type finalAnswerStream struct {
	core     streamCore
	state    *turnState
	itemPath string
}

func (s *finalAnswerStream) AppendText(ctx context.Context, chunk string) error {
	if chunk == "" {
		return nil
	}
	s.core.mu.Lock()
	s.core.appendLocked(chunk)
	s.core.mu.Unlock()
	s.state.emitItemDelta(ctx, s.itemPath, DeltaChannelText, chunk)
	return nil
}

func (s *finalAnswerStream) SetFinalText(text string) {
	s.core.mu.Lock()
	s.core.finalText = &text
	s.core.mu.Unlock()
}
