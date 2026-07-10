// Package loom 是 agent 框架的核心抽象层。
//
// 目标:把异构 agent 实现(手写编排 / 任意 LLM SDK)的输出编织成
// 统一的事件流,再 fan-out 给多个下游(数据库 / WebSocket / log / otel)。
//
// # 当前阶段:LLM 抽象
//
// ChatModel 接口提供同步 / 流式两种调用模式,统一 reasoning_content 字段语义。
// 具体 provider 实现在 pkg/loom/providers/ 子包(目前已支持 DeepSeek)。
//
// # 后续路线
//
//   - Gateway / Writer:agent 业务方调用的高层领域 API
//   - Sink / Repository:可插拔的下游(落库、推送、召回)
//   - 内置 otel 埋点
//   - 更多 provider:OpenAI / Anthropic / 火山引擎等
package loom
