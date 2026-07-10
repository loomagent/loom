package loom

import (
	"context"
	"sync"
)

// MemorySink 在内存收集所有事件。线程安全。
//
// 主要用途:
//   - 单元测试:断言事件序列
//   - 本地调试:回看完整事件流
//   - 离线回放:把事件序列喂给其它 Sink
//
// 同一实例可被多个并发 Turn 共享(内部加锁);Reset 可在跨 Turn 复用时清空。
type MemorySink struct {
	mu       sync.Mutex
	started  []ItemStartedEvent
	deltas   []ItemDeltaEvent
	finished []ItemFinishedEvent
	llmCalls []LLMCalledEvent
}

// NewMemorySink 构造空 MemorySink。
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

// StartedEvents 返回所有 ItemStartedEvent 的副本(线程安全)。
func (s *MemorySink) StartedEvents() []ItemStartedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ItemStartedEvent, len(s.started))
	copy(out, s.started)
	return out
}

// DeltaEvents 返回所有 ItemDeltaEvent 的副本(线程安全)。
func (s *MemorySink) DeltaEvents() []ItemDeltaEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ItemDeltaEvent, len(s.deltas))
	copy(out, s.deltas)
	return out
}

// FinishedEvents 返回所有 ItemFinishedEvent 的副本(线程安全)。
func (s *MemorySink) FinishedEvents() []ItemFinishedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ItemFinishedEvent, len(s.finished))
	copy(out, s.finished)
	return out
}

// LLMCalls 返回所有 LLMCalledEvent 的副本(线程安全)。
func (s *MemorySink) LLMCalls() []LLMCalledEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LLMCalledEvent, len(s.llmCalls))
	copy(out, s.llmCalls)
	return out
}

// Reset 清空所有事件(同 sink 跨多个 Turn 复用前调用)。
func (s *MemorySink) Reset() {
	s.mu.Lock()
	s.started = nil
	s.deltas = nil
	s.finished = nil
	s.llmCalls = nil
	s.mu.Unlock()
}

// 编译期接口断言。
var _ Sink = (*MemorySink)(nil)
