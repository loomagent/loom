package loom

import (
	"context"
	"strings"
	"sync"
)

// streamCore is the accumulation state shared by streaming Items.
type streamCore struct {
	mu        sync.Mutex
	accumText strings.Builder
	// finalText is the value SetFinalText set; nil means the accumulated text is used.
	finalText *string
}

// appendLocked is called with mu already held.
func (c *streamCore) appendLocked(chunk string) {
	c.accumText.WriteString(chunk)
}

// finalize returns the final text: the SetFinalText value, or the accumulated one.
func (c *streamCore) finalize() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finalText != nil {
		return *c.finalText
	}
	return c.accumText.String()
}

// itemTextStream implements both ReasoningStream and FinalAnswerStream. The two
// interfaces have the same method set, so one type satisfies both and neither
// needs an identical copy of its own.
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
