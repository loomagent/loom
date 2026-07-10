package loom

import "context"

// Sink agent 框架的下游事件消费端口。
//
// 实现可插拔:
//   - sinks/memory:测试用,内存收集事件
//   - sinks/log:zap 结构化日志
//   - sinks/tee:多个 Sink 组合 fan-out
//   - 业务侧实现:EntSink(落库)/ WSSink(推前端)/ ProtoStreamSink 等
//
// Sink 失败的处理由 RunOptions.OnSinkErr 决定:
//   - 默认 swallow + 回调 log warn,主流程继续
//   - StrictSink=true 时任一失败立即让整 Turn 失败
//
// Sink 实现需要线程安全:同一 Turn 在一个 goroutine 内串行调用,
// 但不同 Sink 实现可能被多个并发 Turn 共享。
type Sink interface {
	ItemStarted(ctx context.Context, ev ItemStartedEvent) error
	ItemDelta(ctx context.Context, ev ItemDeltaEvent) error
	ItemFinished(ctx context.Context, ev ItemFinishedEvent) error
	LLMCalled(ctx context.Context, ev LLMCalledEvent) error
}
