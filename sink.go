package loom

import "context"

// Sink is where an agent framework's downstream events are consumed.
//
// Implementations are pluggable:
//   - sinks/memory: for tests, collecting events in memory
//   - sinks/log: structured logging through zap
//   - sinks/tee: fan-out to several Sinks
//   - product code: an EntSink for persistence, a WSSink pushing to a frontend,
//     a ProtoStreamSink, and so on
//
// RunOptions.OnSinkErr decides what a sink failure means:
//   - by default it is swallowed and reported through the callback, and the main
//     flow continues
//   - with StrictSink=true any failure fails the whole Turn at once
//
// A Sink implementation must be safe for concurrent use. One Turn calls its sinks
// serially from a single goroutine, but one Sink may be shared by several Turns
// at the same time.
type Sink interface {
	ItemStarted(ctx context.Context, ev ItemStartedEvent) error
	ItemDelta(ctx context.Context, ev ItemDeltaEvent) error
	ItemFinished(ctx context.Context, ev ItemFinishedEvent) error
	LLMCalled(ctx context.Context, ev LLMCalledEvent) error
}
