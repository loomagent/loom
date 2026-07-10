package loom

import "errors"

// ErrUnsupported provider 不支持请求中某个字段的语义。
// 通常 provider 应静默忽略不支持的字段(forward-compat),
// 仅在调用方明确要求且 provider 完全无法替代时返回此错。
var ErrUnsupported = errors.New("loom: provider 不支持此请求")

// ErrTurnClosed Turn 已封口或被外部终结,写入被拒。
// 触发场景:
//   - executor 已通过 FinalAnswer / StreamFinalAnswer 自封口
//   - 外部 cancel / 超时 / dispatcher markFailed
//
// agent 收到此 error 应静默退出。
var ErrTurnClosed = errors.New("loom: turn closed")

// ErrHostShutdown runtime host 正在优雅停机,导致当前 turn 被取消。
var ErrHostShutdown = errors.New("loom: host shutdown")

// ErrExternalCancel 外部权威控制面要求取消当前 turn/run。
var ErrExternalCancel = errors.New("loom: external cancel")

// ErrContentFilter LLM 内容审核截断输出。
// handler 检测到 FinishReasonContentFilter 时 return 此错;
// Run 内核 errors.Is 识别 → Status=failed, CloseReason.Code="content_filter"。
var ErrContentFilter = errors.New("loom: content filter")

// ErrSensitiveContentRisk 表示 provider 在请求建立阶段拒绝本次模型调用,
// 原因是输入/上下文触发了敏感内容风控。
//
// 与 ErrContentFilter 的区别:
//   - ErrContentFilter 对应模型已经返回 finish_reason=content_filter;
//   - ErrSensitiveContentRisk 对应 provider 直接返回错误(常见为 HTTP 400),
//     调用点拿不到 ChatResponse / FinishReason。
//
// provider 应把自己的官方错误类型映射到此 sentinel,上层 policy 不应匹配
// provider 私有文案。例如 DeepSeek 官方 "Content Exists Risk" 由 deepseek
// provider 负责识别。
var ErrSensitiveContentRisk = errors.New("loom: sensitive content risk")

// ErrOutputTruncated LLM 输出被截断(撞 max_tokens 或模型自身输出上限)。
//
// 触发场景对应 OpenAI / DeepSeek / Anthropic 的 finish_reason="length":
//   - 业务方设的 max_tokens 参数限制
//   - 模型自身输出 token 上限(context window 剩余空间不够)
//
// 注意:输入 prompt 超 context 是另一回事 — provider 直接返 400,
// 走不到 stream,handler 拿到的是 model.Stream() 返回的 err,不是此 sentinel。
//
// handler 检测到 FinishReasonLength 时 return 此错;
// Run 内核 errors.Is 识别 → Status=failed, CloseReason.Code="output_truncated"。
var ErrOutputTruncated = errors.New("loom: output truncated")
