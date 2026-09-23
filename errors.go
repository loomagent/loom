package loom

import "errors"

// ErrUnsupportedCapability describes a local capability/configuration restriction.
// It never establishes upstream model support; capability probes must reach the
// provider independently of stored declarations and adapter assumptions.
var ErrUnsupportedCapability = errors.New("loom: provider does not support this request")

// ErrFinalAnswerInProgress means another FinalAnswer or StreamFinalAnswer holds the commit, so
// this one is refused without touching the Turn. It is deliberately not ErrTurnClosed: that error
// means the Turn is already closed and is treated as a cancellation, which would misreport a Turn
// that is still open and, on a retry, still writable.
var ErrFinalAnswerInProgress = errors.New("loom: final answer already in progress")

// ErrTurnClosed means the Turn is sealed or was terminated externally, so the
// write is refused. It happens when:
//   - the handler already sealed the Turn with FinalAnswer / StreamFinalAnswer
//   - an external cancel, a timeout, or a dispatcher markFailed closed it
//
// An agent that receives it should exit quietly.
var ErrTurnClosed = errors.New("loom: turn closed")

// ErrHostShutdown means the runtime host is shutting down gracefully, which
// cancelled the current turn.
var ErrHostShutdown = errors.New("loom: host shutdown")

// ErrExternalCancel means an authoritative external control plane required the
// current turn or run to be cancelled.
var ErrExternalCancel = errors.New("loom: external cancel")

// ErrContentFilter means the model's content moderation cut the output short. A
// handler returns it on FinishReasonContentFilter; the Run core recognises it
// with errors.Is and records Status=failed with
// CloseReason.Code="content_filter".
var ErrContentFilter = errors.New("loom: content filter")

// ErrSensitiveContentRisk means the provider refused the call while the request
// was being established, because the input or context tripped sensitive-content
// moderation.
//
// How it differs from ErrContentFilter:
//   - ErrContentFilter means the model already returned
//     finish_reason=content_filter
//   - ErrSensitiveContentRisk means the provider returned an error directly,
//     usually an HTTP 400, so the call site has neither a ChatResponse nor a
//     FinishReason
//
// A provider maps its own official error type onto this sentinel, and policies
// above it must not match a provider's private wording. Recognising DeepSeek's
// official "Content Exists Risk", for example, is the deepseek provider's job.
var ErrSensitiveContentRisk = errors.New("loom: sensitive content risk")

// ErrOutputTruncated means the model's output was cut short, whether by
// max_tokens or by the model's own output limit.
//
// It matches finish_reason="length" from OpenAI, DeepSeek, and Anthropic:
//   - the max_tokens limit the caller set
//   - the model's own output token limit, when the context window has no room left
//
// An input prompt exceeding the context is a different matter: the provider
// returns a 400 before the stream starts, so the handler receives the error from
// model.Stream() rather than this sentinel.
//
// A handler returns it on FinishReasonLength; the Run core recognises it with
// errors.Is and records Status=failed with
// CloseReason.Code="output_truncated".
var ErrOutputTruncated = errors.New("loom: output truncated")
