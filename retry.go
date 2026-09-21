package loom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	backoff "github.com/cenkalti/backoff/v5"
)

// ErrorClass is the semantic class of an LLM provider error.
//
// A provider implements ErrorClassifier to translate its own transport errors, an
// *goseek.APIError or a net or ctx error, into a common ErrorClass, which the framework
// uses to decide its retry policy.
//
// Specific status codes are not exposed, because providers disagree about what they
// mean; everything is reduced to four classes.
type ErrorClass int

const (
	// ErrorClassUnknown is an unrecognised error, treated like Transient and retried
	// conservatively.
	ErrorClassUnknown ErrorClass = iota
	// ErrorClassTransient is a temporary failure, retried a bounded number of times and
	// capped by MaxRetries: a 5xx, a flaky network, a temporary DNS failure, a connection
	// reset.
	ErrorClassTransient
	// ErrorClassRateLimit is throttling. It does not count against MaxRetries, but it is
	// bounded by both a separate elapsed budget and the parent ctx: an HTTP 429, or a
	// provider's own rate-limit status.
	ErrorClassRateLimit
	// ErrorClassPermanent gives up at once, with no retry: 401, 402, and 403 for
	// authentication or insufficient balance, a 400 bad request, a content-moderation
	// refusal, a missing model, and ctx.Canceled or ctx.DeadlineExceeded, which mean the
	// caller stopped it.
	ErrorClassPermanent
)

// ErrorClassifier translates a provider's own error into an ErrorClass.
//
// Every provider must implement one. There is no default classification, which keeps an
// authentication or argument error from being mistaken for a recoverable transient one.
// A provider's implementation usually looks like:
//
//	type myClassifier struct{}
//	func (myClassifier) ClassifyError(err error) loom.ErrorClass {
//	    var apiErr *myprovider.APIError
//	    if errors.As(err, &apiErr) { ... return loom.ErrorClassXxx }
//	    if errors.Is(err, context.Canceled) { return loom.ErrorClassPermanent }
//	    return loom.ErrorClassTransient
//	}
type ErrorClassifier interface {
	ClassifyError(err error) ErrorClass
}

// RetryMode bounds how Transient and Unknown errors are retried.
//
// RateLimit is bounded by RateLimitMaxElapsed and ctx, and Permanent always returns at
// once; this mode affects Transient and Unknown only. The zero value behaves as
// RetryModeFinite.
type RetryMode string

const (
	RetryModeFinite       RetryMode = "finite"
	RetryModeUntilContext RetryMode = "until_context"
	RetryModeDisabled     RetryMode = "disabled"
)

// ErrAttemptTimeout means one attempt's own deadline expired while the parent ctx the
// caller passed is still live. It is a recoverable error, and must not be confused with
// the whole Turn's deadline.
var ErrAttemptTimeout = errors.New("loom: model attempt timeout")

// AttemptTimeoutError keeps the original text of a single attempt's timeout and exposes
// ErrAttemptTimeout through Unwrap, so the underlying context.DeadlineExceeded is not
// mistaken for the parent's own deadline.
type AttemptTimeoutError struct {
	Timeout time.Duration
	Cause   error
}

func (e *AttemptTimeoutError) Error() string {
	if e == nil {
		return ErrAttemptTimeout.Error()
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s after %s: %v", ErrAttemptTimeout, e.Timeout, e.Cause)
	}
	return fmt.Sprintf("%s after %s", ErrAttemptTimeout, e.Timeout)
}

func (e *AttemptTimeoutError) Unwrap() error { return ErrAttemptTimeout }

// ClassifiedError is the typed model error the retry schedule finally returns. An
// orchestrator above it can decide whether to switch models or extend a business deadline
// without parsing provider text or an SDK's private types.
type ClassifiedError struct {
	Class    ErrorClass
	Attempts int
	Err      error
}

func (e *ClassifiedError) Error() string {
	if e == nil {
		return "loom: classified model error"
	}
	return fmt.Sprintf("loom: model call failed (class=%s attempts=%d): %v", e.Class, e.Attempts, e.Err)
}

func (e *ClassifiedError) Unwrap() error { return e.Err }

// ErrorClassOf reads the common error class from an error chain.
func ErrorClassOf(err error) (ErrorClass, bool) {
	var classified *ClassifiedError
	if !errors.As(err, &classified) {
		return ErrorClassUnknown, false
	}
	return classified.Class, true
}

func (c ErrorClass) String() string {
	switch c {
	case ErrorClassUnknown:
		return "unknown"
	case ErrorClassTransient:
		return "transient"
	case ErrorClassRateLimit:
		return "rate_limit"
	case ErrorClassPermanent:
		return "permanent"
	default:
		return fmt.Sprintf("error_class_%d", int(c))
	}
}

// RetryConfig is the shared retry schedule configuration, used by every provider.
//
// A zero field takes its default from DefaultRetryConfig when ChatWithRetry or
// StreamWithRetry starts.
type RetryConfig struct {
	// Mode decides whether Transient and Unknown errors are retried a bounded number of
	// times, retried until ctx ends, or not retried at all. The zero value means
	// RetryModeFinite.
	Mode RetryMode

	// MaxRetries caps the retries of Transient errors; the first call does not count, so
	// MaxRetries=2 means at most three attempts, one initial and two retries. RateLimit is
	// unbounded by count but bounded by the elapsed budget, and Permanent is skipped
	// entirely.
	MaxRetries int

	// RateLimitMaxElapsed bounds how long one logical call may wait from its first 429. It
	// applies together with the parent ctx, and whichever comes first ends the retries.
	RateLimitMaxElapsed time.Duration

	// InitialBackoff is the first backoff interval, where exponential backoff starts.
	InitialBackoff time.Duration

	// MaxBackoff caps the backoff; exponential growth never exceeds it.
	MaxBackoff time.Duration

	// PerCallTimeout bounds each single Chat call through ctx. Only ChatWithRetry uses
	// it: StreamWithRetry covers connection setup and the first frame, and the consumer
	// owns the rest of the stream's life. StreamLLMToStep applies its own per-stream
	// timeout by default.
	PerCallTimeout time.Duration

	// AttemptLimiter takes a permit around each real HTTP or streaming attempt. No permit
	// is held during backoff. Models sharing one provider credential should share an
	// AttemptMeta.QuotaKey.
	AttemptLimiter AttemptLimiter
	AttemptMeta    AttemptMeta
}

// DefaultRetryConfig returns the defaults the framework recommends.
//
// Product code rarely needs to change them; they are a compromise based on how most
// hosted LLM APIs throttle and flake. When it does, a development environment that wants
// to fail fast sets MaxRetries=0, and a long prompt deserves a longer PerCallTimeout.
func DefaultRetryConfig() *RetryConfig {
	return &RetryConfig{
		Mode:                RetryModeFinite,
		MaxRetries:          2,
		RateLimitMaxElapsed: 10 * time.Minute,
		InitialBackoff:      time.Second,
		MaxBackoff:          30 * time.Second,
		PerCallTimeout:      5 * time.Minute,
	}
}

// applyDefaults fills zero fields with their defaults, in place.
func (c *RetryConfig) applyDefaults() {
	def := DefaultRetryConfig()
	if c.Mode == "" {
		c.Mode = def.Mode
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = def.MaxRetries
	}
	if c.RateLimitMaxElapsed == 0 {
		c.RateLimitMaxElapsed = def.RateLimitMaxElapsed
	}
	if c.InitialBackoff == 0 {
		c.InitialBackoff = def.InitialBackoff
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = def.MaxBackoff
	}
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = def.PerCallTimeout
	}
}

func (c *RetryConfig) validate() error {
	switch c.Mode {
	case RetryModeFinite, RetryModeUntilContext, RetryModeDisabled:
		return nil
	default:
		return fmt.Errorf("loom: unknown retry mode %q", c.Mode)
	}
}

func normalizedRetryConfig(config *RetryConfig) (*RetryConfig, error) {
	if config == nil {
		config = DefaultRetryConfig()
	} else {
		copy := *config
		config = &copy
		config.applyDefaults()
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return config, nil
}

// newBackoff builds the backoff schedule. backoff/v5 bounds no total duration by default;
// the outer ctx and MaxRetries do that.
func (c *RetryConfig) newBackoff() *backoff.ExponentialBackOff {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = c.InitialBackoff
	bo.MaxInterval = c.MaxBackoff
	return bo
}

// ChatWithRetry is the shared retry schedule around one synchronous Chat call, which a
// provider invokes inside its own Chat:
//
//	func (m *Model) Chat(ctx context.Context, req loom.ChatRequest) (*loom.ChatResponse, error) {
//	    return loom.ChatWithRetry(ctx, m.classifier, m.retryCfg, func(callCtx context.Context) (*loom.ChatResponse, error) {
//	        return m.chatRaw(callCtx, req)
//	    })
//	}
//
// Behaviour:
//   - every attempt of fn gets a callCtx wrapped with PerCallTimeout
//   - errors are classified: Transient counts and retries, RateLimit does not count but
//     runs against an elapsed budget, Permanent gives up at once
//   - a cancelled outer ctx gives up at once, treating ctx.Err as Permanent
//
// classifier must not be nil. The framework provides no default classifier, which keeps a
// permanent error from being retried.
func ChatWithRetry(
	ctx context.Context,
	classifier ErrorClassifier,
	cfg *RetryConfig,
	fn func(callCtx context.Context) (*ChatResponse, error),
) (*ChatResponse, error) {
	if classifier == nil {
		return nil, errors.New("loom.ChatWithRetry: classifier must not be nil")
	}
	var err error
	cfg, err = normalizedRetryConfig(cfg)
	if err != nil {
		return nil, err
	}

	nonRateLimitAttempts := 0
	totalAttempts := 0
	lastClass := ErrorClassUnknown
	var rateLimitStarted time.Time
	resp, err := backoff.Retry(ctx, func() (*ChatResponse, error) {
		totalAttempts++
		permit, acquireErr := acquireAttempt(ctx, cfg, rateLimitStarted)
		if acquireErr != nil {
			lastClass = ErrorClassPermanent
			return nil, backoff.Permanent(acquireErr)
		}
		callCtx, cancel := context.WithTimeout(ctx, cfg.PerCallTimeout)
		resp, err := fn(callCtx)
		attemptCtxErr := callCtx.Err()
		parentCtxErr := ctx.Err()
		cancel()
		if err != nil {
			if parentCtxErr != nil {
				lastClass = ErrorClassPermanent
				finishAttempt(permit, AttemptResult{ErrorClass: lastClass, Err: parentCtxErr})
				return nil, backoff.Permanent(parentCtxErr)
			}
			// While the parent ctx is still live, a deadline or cancellation inside the
			// model call belongs to this attempt, not to the whole task.
			if attemptCtxErr != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				err = &AttemptTimeoutError{Timeout: cfg.PerCallTimeout, Cause: err}
				lastClass = ErrorClassTransient
				finishAttempt(permit, AttemptResult{ErrorClass: lastClass, Err: err})
				return nil, classifyForBackoff(err, ErrorClassTransient, cfg, &nonRateLimitAttempts, &rateLimitStarted)
			}
			lastClass = classifier.ClassifyError(err)
			finishAttempt(permit, failedAttempt(classifier, lastClass, err))
			return nil, classifyForBackoff(err, lastClass, cfg, &nonRateLimitAttempts, &rateLimitStarted)
		}
		finishAttempt(permit, AttemptResult{Success: true})
		return resp, nil
	},
		backoff.WithBackOff(cfg.newBackoff()),
	)
	if err == nil {
		return resp, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, &ClassifiedError{Class: lastClass, Attempts: totalAttempts, Err: err}
}

// StreamWithRetry is the shared retry schedule around one Stream call plus a first-frame
// liveness probe, which a provider invokes inside its own Stream:
//
//	func (m *Model) Stream(ctx context.Context, req loom.ChatRequest) (loom.Stream, error) {
//	    return loom.StreamWithRetry(ctx, m.classifier, m.retryCfg, func(streamCtx context.Context) (loom.Stream, error) {
//	        return m.streamRaw(streamCtx, req)
//	    })
//	}
//
// Behaviour:
//   - fn is retried until it returns a usable Stream; a Stream() failure counts as a retry
//   - once there is a Stream, the first frame is prefetched as a liveness probe
//   - a non-EOF error on the first frame closes the stream and retries as if Stream() had
//     failed
//   - a successful first frame, or EOF, is wrapped in prefixStream, so the consumer's
//     first Recv returns the probed frame and later ones go through to inner.Recv
//   - a Stream created by fn is not under PerCallTimeout; StreamLLMToStep applies its own
//     timeout to a whole stream further out, so a provider's internal retry cannot reset
//     the overall deadline
//
// streamCtx equals the outer ctx, with no extra timeout. The fn implementation owns
// connect timeouts and the like, through its HTTP client configuration.
func StreamWithRetry(
	ctx context.Context,
	classifier ErrorClassifier,
	cfg *RetryConfig,
	fn func(streamCtx context.Context) (Stream, error),
) (Stream, error) {
	if classifier == nil {
		return nil, errors.New("loom.StreamWithRetry: classifier must not be nil")
	}
	var err error
	cfg, err = normalizedRetryConfig(cfg)
	if err != nil {
		return nil, err
	}

	nonRateLimitAttempts := 0
	totalAttempts := 0
	lastClass := ErrorClassUnknown
	var rateLimitStarted time.Time
	stream, err := backoff.Retry(ctx, func() (Stream, error) {
		totalAttempts++
		permit, acquireErr := acquireAttempt(ctx, cfg, rateLimitStarted)
		if acquireErr != nil {
			lastClass = ErrorClassPermanent
			return nil, backoff.Permanent(acquireErr)
		}
		stream, err := fn(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				lastClass = ErrorClassPermanent
				finishAttempt(permit, AttemptResult{ErrorClass: lastClass, Err: ctxErr})
				return nil, backoff.Permanent(ctxErr)
			}
			lastClass = classifier.ClassifyError(err)
			finishAttempt(permit, failedAttempt(classifier, lastClass, err))
			return nil, classifyForBackoff(err, lastClass, cfg, &nonRateLimitAttempts, &rateLimitStarted)
		}
		// Probe the first frame: Stream() not returning an error does not mean the server
		// accepted the request, because some providers only return a 4xx or 5xx body with
		// the first chunk.
		first, recvErr := stream.Recv()
		if recvErr != nil && !errors.Is(recvErr, io.EOF) {
			_ = stream.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				lastClass = ErrorClassPermanent
				finishAttempt(permit, AttemptResult{ErrorClass: lastClass, Err: ctxErr})
				return nil, backoff.Permanent(ctxErr)
			}
			lastClass = classifier.ClassifyError(recvErr)
			finishAttempt(permit, failedAttempt(classifier, lastClass, recvErr))
			return nil, classifyForBackoff(recvErr, lastClass, cfg, &nonRateLimitAttempts, &rateLimitStarted)
		}
		firstEOF := errors.Is(recvErr, io.EOF)
		wrapped := &prefixStream{inner: stream, first: first, firstEOF: firstEOF, permit: permit, classifier: classifier}
		if firstEOF {
			wrapped.finish(AttemptResult{Success: true})
		}
		return wrapped, nil
	},
		backoff.WithBackOff(cfg.newBackoff()),
	)
	if err == nil {
		return stream, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, &ClassifiedError{Class: lastClass, Attempts: totalAttempts, Err: err}
}

// classifyForBackoff turns the original error into the permanent-or-not answer backoff
// wants.
//
//   - Permanent, ctx.Canceled, ctx.DeadlineExceeded → give up at once, backoff.Permanent
//   - RateLimit → pass through without counting, bounded by the elapsed budget and ctx
//   - Transient → count it, and escalate to Permanent past MaxRetries; otherwise pass
//     through and retry
//   - Unknown → treat as Transient
//
// nonRateLimitAttempts is the running count of non-RateLimit errors, maintained by the
// caller.
func classifyForBackoff(
	err error,
	class ErrorClass,
	cfg *RetryConfig,
	nonRateLimitAttempts *int,
	rateLimitStarted *time.Time,
) error {
	if cfg.Mode == RetryModeDisabled {
		return backoff.Permanent(err)
	}
	switch class {
	case ErrorClassPermanent:
		return backoff.Permanent(err)
	case ErrorClassRateLimit:
		if rateLimitStarted.IsZero() {
			*rateLimitStarted = time.Now()
		}
		if cfg.RateLimitMaxElapsed > 0 && time.Since(*rateLimitStarted) >= cfg.RateLimitMaxElapsed {
			return backoff.Permanent(fmt.Errorf("rate limit retry budget exhausted after %s: %w", cfg.RateLimitMaxElapsed, err))
		}
		return err
	case ErrorClassTransient, ErrorClassUnknown:
		if cfg.Mode == RetryModeUntilContext {
			return err
		}
		*nonRateLimitAttempts++
		if *nonRateLimitAttempts > cfg.MaxRetries {
			return backoff.Permanent(fmt.Errorf("retry exhausted after %d attempts: %w", *nonRateLimitAttempts, err))
		}
		return err
	default:
		return backoff.Permanent(err)
	}
}

// prefixStream puts the prefetched first frame in front of the inner stream.
//
// The consumer's Recv returns the prefetched frame first, or EOF directly, and later
// calls go through to inner.Recv. Close goes through to inner.
type prefixStream struct {
	inner         Stream
	first         *Chunk
	firstEOF      bool
	firstConsumed bool
	permit        AttemptPermit
	classifier    ErrorClassifier
	finishOnce    sync.Once
}

func (s *prefixStream) Recv() (*Chunk, error) {
	if !s.firstConsumed {
		s.firstConsumed = true
		if s.firstEOF {
			return nil, io.EOF
		}
		return s.first, nil
	}
	chunk, err := s.inner.Recv()
	if errors.Is(err, io.EOF) {
		s.finish(AttemptResult{Success: true})
	} else if err != nil {
		class := ErrorClassTransient
		if s.classifier != nil {
			class = s.classifier.ClassifyError(err)
		}
		s.finish(failedAttempt(s.classifier, class, err))
	}
	return chunk, err
}

func (s *prefixStream) Close() error {
	err := s.inner.Close()
	s.finish(AttemptResult{ErrorClass: ErrorClassPermanent, Err: err})
	return err
}

func (s *prefixStream) finish(result AttemptResult) {
	s.finishOnce.Do(func() { finishAttempt(s.permit, result) })
}

func acquireAttempt(ctx context.Context, cfg *RetryConfig, rateLimitStarted time.Time) (AttemptPermit, error) {
	if cfg.AttemptLimiter == nil {
		return nil, nil
	}
	if rateLimitStarted.IsZero() || cfg.RateLimitMaxElapsed <= 0 {
		return cfg.AttemptLimiter.Acquire(ctx, cfg.AttemptMeta)
	}
	remaining := cfg.RateLimitMaxElapsed - time.Since(rateLimitStarted)
	if remaining <= 0 {
		return nil, fmt.Errorf("rate limit retry budget exhausted after %s", cfg.RateLimitMaxElapsed)
	}
	waitCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	permit, err := cfg.AttemptLimiter.Acquire(waitCtx, cfg.AttemptMeta)
	if err != nil && ctx.Err() == nil && waitCtx.Err() != nil {
		return nil, fmt.Errorf("rate limit retry budget exhausted after %s: %w", cfg.RateLimitMaxElapsed, err)
	}
	return permit, err
}

func finishAttempt(permit AttemptPermit, result AttemptResult) {
	if permit != nil {
		permit.Finish(result)
	}
}

func retryAfter(classifier ErrorClassifier, err error) time.Duration {
	provider, ok := classifier.(RetryAfterClassifier)
	if !ok {
		return 0
	}
	return max(time.Duration(0), provider.RetryAfter(err))
}

func failedAttempt(classifier ErrorClassifier, class ErrorClass, err error) AttemptResult {
	result := AttemptResult{ErrorClass: class, Err: err, RetryAfter: retryAfter(classifier, err)}
	provider, ok := classifier.(ServiceUnavailableClassifier)
	result.ServiceUnavailable = ok && provider.IsServiceUnavailable(err)
	return result
}
