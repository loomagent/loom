# Provider content-policy failures

Chat-completions adapters classify explicit provider moderation as
`ErrSensitiveContentRisk`. Output finish reasons map to `FinishReasonContentFilter`
or a typed moderation error. Loom turns close as `failed / content_filter` for
either `ErrSensitiveContentRisk` or `ErrContentFilter`.

| Provider | Request/error evidence | Output evidence |
| --- | --- | --- |
| DeepSeek | HTTP 400 with the exact `Content Exists Risk` message, optionally followed by a nonempty `request_id` suffix; conflicting business types/codes are excluded. SSE envelopes require `type` and `code` to be `invalid_request_error`. | `finish_reason=content_filter` |
| Zhipu | Business code `1301`, string or numeric, retaining HTTP status and request ID separately. Compatible SSE error envelopes also retain the original event. | `finish_reason=sensitive` or `content_filter` |
| Ark | HTTP 400 or in-stream SDK error with an explicit `SensitiveContentDetected`, input/output text/image/video/audio sensitive-content code, or input/output text/image `RiskDetection`. A nonempty dot-delimited policy subcategory is supported. | `finish_reason=content_filter` |
| OpenRouter | Error code 403 with `metadata.error_type=content_policy_violation` or `refusal`; legacy moderation requires the complete documented metadata shape: reasons, flagged input, provider name, model slug. Typed non-policy errors override legacy fields. | `finish_reason=content_filter`; `finish_reason=error` remains failure even without moderation evidence. |

OpenRouter errors may arrive as HTTP 4xx, an HTTP-200 top-level error, a unary
choice-local error alongside partial content, or a top-level SSE error after HTTP
200. `APIError.StatusCode` records the transport status; `APIError.Code` records
the error-body status used by retry classification. SDK HTTP errors and SSE events
remain reachable with `errors.As`; generation/request IDs and provider error
codes remain diagnostic fields. `APIError.RawJSON()` provides opt-in access to
the original error object. Moderation metadata is not printed by
`APIError.Error()` because it can contain user input.

Ark SSE objects are decoded by meaning rather than JSON field order. The SDK's
prefix-based error detection alone misses envelopes with a leading generation
ID. Request IDs from the stream response header are retained in the SDK cause.

Ordinary parameter, authentication, permission, balance and availability errors
retain their operational classification. Error messages that merely quote a
policy code are not evidence of moderation. Ark's `ContentSecurityDetectionError`
means the moderation service failed; it is not a content-policy verdict.
OpenRouter prompt-injection guardrails with only `patterns` metadata are also
not treated as content moderation without the explicit signals above.

Moderation errors are permanent within a provider's retry policy. A host may
apply its configured sensitive-content fallback before final delivery; Loom's
final-answer streaming helper does not replay partial output or switch models.
Failed delivery leaves partial items visible and never commits a final answer.
Structured-output callers must inspect `FinishReasonContentFilter` before parsing
JSON, and must not retry a moderation error as malformed output.

## Offline acceptance

`providers/content_policy_test.go` uses actual adapters, official SDKs, strict
JSON-v2 fixtures and in-memory HTTP/SSE servers inside `synctest` bubbles. It
covers request rejection, in-stream errors before/after partial output, output
finish reasons, OpenRouter HTTP-200 failures, JSON field ordering, and operational
400/403/balance counterexamples. Captured Ark HTTP/SSE refusals also cover
nonempty text alongside `content_filter` and an explicit zero-token usage record;
only model and diagnostic identifiers are anonymized. No provider endpoint or
credential is used. Successful `stop` replies, including natural-language refusals,
are not classified by their wording.

```sh
LOOM_LIVE_ENV= go test -race ./providers ./providers/...
LOOM_LIVE_ENV= go test -race -coverpkg=./... -coverprofile=coverage.out ./...
bash scripts/check-coverage.sh coverage.out
```

CI measures cross-package execution with `-coverpkg=./...`, so these adapter
acceptance tests count the production code they execute. The 90% statement
coverage floor is retained; it does not replace failure-branch review.

## Protocol sources

- [Zhipu business errors and SSE termination](https://docs.bigmodel.cn/cn/api/api-code)
- [Ark error codes](https://docs.volcengine.com/docs/ark/error-codes?lang=zh)
- [Ark chat-completions finish reasons](https://docs.volcengine.com/docs/ark/chat-api?lang=zh)
- [DeepSeek chat-completions finish reasons](https://api-docs.deepseek.com/api/create-chat-completion)
- [OpenRouter error and SSE contracts](https://openrouter.ai/docs/api/reference/errors)

DeepSeek's HTTP message and request-ID suffix were captured from the dev failure
tracked by [wolotech/product-issues#116](https://github.com/wolotech/product-issues/issues/116).
