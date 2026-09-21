// Package loom is the core abstraction layer of an agent framework.
//
// It weaves the output of heterogeneous agent implementations (hand-written
// orchestration / any LLM SDK) into a unified event stream, fanned out to several
// downstreams (database / WebSocket / log / otel).
//
// # LLM abstraction
//
// The ChatModel interface offers both synchronous and streaming call modes and
// unifies what the reasoning_content field means. Concrete providers live in the
// providers/ subpackages.
package loom
