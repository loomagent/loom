package loom

import (
	"context"
	"sync"
)

// MemorySink collects every event in memory. It is safe for concurrent use.
//
// Its main uses:
//   - unit tests: assert on the event sequence
//   - local debugging: look back over the complete event stream
//   - offline replay: feed the event sequence to another Sink
//
// Several concurrent Turns may share one instance, since it locks internally.
// Reset clears it before reuse across Turns.
type MemorySink struct {
	mu       sync.Mutex
	started  []ItemStartedEvent
	deltas   []ItemDeltaEvent
	finished []ItemFinishedEvent
	llmCalls []LLMCalledEvent
}

// NewMemorySink returns an empty MemorySink.
func NewMemorySink() *MemorySink {
	return &MemorySink{}
}

func (s *MemorySink) ItemStarted(_ context.Context, ev ItemStartedEvent) error {
	s.mu.Lock()
	s.started = append(s.started, ev)
	s.mu.Unlock()
	return nil
}

func (s *MemorySink) ItemDelta(_ context.Context, ev ItemDeltaEvent) error {
	s.mu.Lock()
	s.deltas = append(s.deltas, ev)
	s.mu.Unlock()
	return nil
}

func (s *MemorySink) ItemFinished(_ context.Context, ev ItemFinishedEvent) error {
	s.mu.Lock()
	s.finished = append(s.finished, ev)
	s.mu.Unlock()
	return nil
}

func (s *MemorySink) LLMCalled(_ context.Context, ev LLMCalledEvent) error {
	s.mu.Lock()
	s.llmCalls = append(s.llmCalls, ev)
	s.mu.Unlock()
	return nil
}

// StartedEvents returns a copy of every ItemStartedEvent. It is safe for
// concurrent use.
func (s *MemorySink) StartedEvents() []ItemStartedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ItemStartedEvent, len(s.started))
	copy(out, s.started)
	return out
}

// DeltaEvents returns a copy of every ItemDeltaEvent. It is safe for concurrent
// use.
func (s *MemorySink) DeltaEvents() []ItemDeltaEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ItemDeltaEvent, len(s.deltas))
	copy(out, s.deltas)
	return out
}

// FinishedEvents returns a copy of every ItemFinishedEvent. It is safe for
// concurrent use.
func (s *MemorySink) FinishedEvents() []ItemFinishedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ItemFinishedEvent, len(s.finished))
	copy(out, s.finished)
	return out
}

// LLMCalls returns a copy of every LLMCalledEvent. It is safe for concurrent
// use.
func (s *MemorySink) LLMCalls() []LLMCalledEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LLMCalledEvent, len(s.llmCalls))
	copy(out, s.llmCalls)
	return out
}

// Reset clears every event. Call it before reusing one sink across several Turns.
func (s *MemorySink) Reset() {
	s.mu.Lock()
	s.started = nil
	s.deltas = nil
	s.finished = nil
	s.llmCalls = nil
	s.mu.Unlock()
}

// Compile-time interface assertions.
var _ Sink = (*MemorySink)(nil)
