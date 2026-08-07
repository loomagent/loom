package loom

import (
	"context"
	"time"
)

// AttemptMeta identifies the real supplier quota consumed by one physical LLM
// request. QuotaKey is an internal credential fingerprint and must never be
// exported as a metric label; QuotaLabel is the stable, non-secret provider ID.
type AttemptMeta struct {
	QuotaKey   string
	QuotaLabel string
	Model      string
}

// AttemptResult closes one physical request attempt. Success means a complete
// Chat response or a Stream consumed to EOF. ErrorClass is meaningful only
// when Success is false.
type AttemptResult struct {
	Success    bool
	ErrorClass ErrorClass
	Err        error
	RetryAfter time.Duration
	// ServiceUnavailable marks an HTTP 503-style provider outage. It opens a
	// shared half-open circuit without teaching AIMD that the credential quota
	// is smaller.
	ServiceUnavailable bool
}

// AttemptPermit is held only while the upstream request is physically in
// flight. Finish implementations must be idempotent because a stream may reach
// EOF and still be explicitly closed by its consumer.
type AttemptPermit interface {
	Finish(result AttemptResult)
}

// AttemptLimiter coordinates physical requests across every model instance
// sharing the same provider credential.
type AttemptLimiter interface {
	Acquire(ctx context.Context, meta AttemptMeta) (AttemptPermit, error)
}

// RetryAfterClassifier optionally extracts the server-requested delay from a
// provider error. Providers without response headers rely on the shared
// limiter's configured cooldown.
type RetryAfterClassifier interface {
	RetryAfter(err error) time.Duration
}

// ServiceUnavailableClassifier identifies provider availability failures that
// need a shared circuit but must not be treated as quota feedback.
type ServiceUnavailableClassifier interface {
	IsServiceUnavailable(err error) bool
}
