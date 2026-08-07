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

// ErrorClass LLM provider 错误的语义分类。
//
// provider 实现 ErrorClassifier 把自己 transport 层错误(如 *goseek.APIError、
// net error、ctx 错误)翻译成统一的 ErrorClass。框架据此决定 retry 策略。
//
// 不暴露具体 status code — 因为不同 provider 的状态语义不一样,统一抽象到 4 类。
type ErrorClass int

const (
	// ErrorClassUnknown 未识别错误,等同 Transient 处理(保守 retry)。
	ErrorClassUnknown ErrorClass = iota
	// ErrorClassTransient 暂时性错误,有限 retry(MaxRetries 控制次数)。
	// 典型:5xx / 网络抖动 / DNS 临时失败 / connection reset。
	ErrorClassTransient
	// ErrorClassRateLimit 限流,不计入 MaxRetries，但受独立 elapsed budget 和
	// 父 ctx 双重约束。
	// 典型:HTTP 429、provider 自定的 RateLimit 状态。
	ErrorClassRateLimit
	// ErrorClassPermanent 立即放弃,不 retry。
	// 典型:401/402/403(auth / 余额不足)、400(bad request)、内容审核拒绝、
	// 模型不存在;以及 ctx.Canceled / ctx.DeadlineExceeded(外层主动终止)。
	ErrorClassPermanent
)

// ErrorClassifier 把 provider 原生 error 翻译成 ErrorClass。
//
// 每个 provider 必须实现一份；没有默认分类，避免把鉴权/参数错误误识别为
// 可恢复的 Transient。Provider 实现一般是:
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

// RetryMode 控制 Transient / Unknown 错误的重试边界。
//
// RateLimit 受 RateLimitMaxElapsed 与 ctx 约束，Permanent 始终立即返回；
// 本模式只影响 Transient / Unknown。零值等价 RetryModeFinite。
type RetryMode string

const (
	RetryModeFinite       RetryMode = "finite"
	RetryModeUntilContext RetryMode = "until_context"
	RetryModeDisabled     RetryMode = "disabled"
)

// ErrAttemptTimeout 表示一次模型 attempt 的局部 deadline 已到，但调用方传入的
// 父 ctx 仍然有效。它是可恢复错误，不能和整条 Turn 的总 deadline 混为一谈。
var ErrAttemptTimeout = errors.New("loom: model attempt timeout")

// AttemptTimeoutError 保留单次超时的原始错误文本，同时通过 Unwrap 暴露
// ErrAttemptTimeout，避免原始 context.DeadlineExceeded 再被误判成父级总超时。
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

// ClassifiedError 是 retry 调度最终返回的类型化模型错误。上层编排器可据此决定
// 是否切换模型、延长到业务总 deadline，避免解析 provider 文本或 SDK 私有类型。
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

// ErrorClassOf 从错误链读取统一模型错误分类。
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

// RetryConfig 通用 retry 调度配置。所有 provider 共享。
//
// 字段为零值时,DefaultRetryConfig 的默认值会在 ChatWithRetry / StreamWithRetry
// 入口生效。
type RetryConfig struct {
	// Mode 控制 Transient / Unknown 错误是有限重试、重试到 ctx 结束，还是关闭重试。
	// 零值按 RetryModeFinite 处理。
	Mode RetryMode

	// MaxRetries Transient 类错误的最大重试次数(首次调用不计)。
	// 例 MaxRetries=2 表示最多尝试 3 次(1 次首调 + 2 次重试)。
	// RateLimit 类不受次数限制但受 elapsed budget 约束，Permanent 类完全跳过。
	MaxRetries int

	// RateLimitMaxElapsed 限制一个逻辑 LLM 调用从首次 429 起最多等待多久。
	// 它与父 ctx 共同生效，先到者终止重试。
	RateLimitMaxElapsed time.Duration

	// InitialBackoff 首次 backoff 间隔。指数退避从此起步。
	InitialBackoff time.Duration

	// MaxBackoff backoff 上限。指数退避不超过此值。
	MaxBackoff time.Duration

	// PerCallTimeout 每次单次 Chat 调用的 ctx 超时。
	// 仅 ChatWithRetry 使用。StreamWithRetry 只负责建连和首帧重试；完整流的
	// 生命周期由消费方控制，StreamLLMToStep 默认施加系统级单次流超时。
	PerCallTimeout time.Duration

	// AttemptLimiter 在每个真实 HTTP/流式 attempt 周围取得 permit。
	// Backoff 期间不持有 permit；同一供应商凭据的模型应共享 AttemptMeta.QuotaKey。
	AttemptLimiter AttemptLimiter
	AttemptMeta    AttemptMeta
}

// DefaultRetryConfig 返回框架推荐默认值。
//
// 业务方一般不需要改这些值 — 这是基于"大多数 SaaS LLM 限流 / 抖动模式"的折中。
// 真要调,例如 dev 环境想快速 fail 改 MaxRetries=0,或长 prompt 给更长 PerCallTimeout。
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

// applyDefaults 对零值字段填默认值(就地修改)。
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

// newBackoff 构造 backoff 调度器。
// backoff/v5 默认不限制总时长 — 总时长由外层 ctx + MaxRetries 控制。
func (c *RetryConfig) newBackoff() *backoff.ExponentialBackOff {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = c.InitialBackoff
	bo.MaxInterval = c.MaxBackoff
	return bo
}

// ChatWithRetry 通用 retry 调度,wrap 一次同步 Chat 调用。Provider 在 Chat 内调:
//
//	func (m *Model) Chat(ctx context.Context, req loom.ChatRequest) (*loom.ChatResponse, error) {
//	    return loom.ChatWithRetry(ctx, m.classifier, m.retryCfg, func(callCtx context.Context) (*loom.ChatResponse, error) {
//	        return m.chatRaw(callCtx, req)
//	    })
//	}
//
// 行为:
//   - 每次尝试 fn 都拿到一个 PerCallTimeout-wrap 的 callCtx
//   - 错误分类:Transient 计数 retry / RateLimit 不计次数但有 elapsed budget / Permanent 立即放弃
//   - 外层 ctx 取消立即放弃(把 ctx.Err 当 Permanent)
//
// classifier 不可为 nil — 框架不提供默认分类器,避免把永久错误错误重试。
func ChatWithRetry(
	ctx context.Context,
	classifier ErrorClassifier,
	cfg *RetryConfig,
	fn func(callCtx context.Context) (*ChatResponse, error),
) (*ChatResponse, error) {
	if classifier == nil {
		return nil, errors.New("loom.ChatWithRetry: classifier 不能为 nil")
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
			// 只要父 ctx 仍有效，模型调用内部出现的 deadline/cancel 就属于
			// 本次 attempt，而不是整条任务的终止信号。
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

// StreamWithRetry 通用 retry 调度,wrap 一次 Stream 调用 + 首帧探活。Provider 在 Stream 内调:
//
//	func (m *Model) Stream(ctx context.Context, req loom.ChatRequest) (loom.Stream, error) {
//	    return loom.StreamWithRetry(ctx, m.classifier, m.retryCfg, func(streamCtx context.Context) (loom.Stream, error) {
//	        return m.streamRaw(streamCtx, req)
//	    })
//	}
//
// 行为:
//   - retry fn 直到拿到合法 Stream(Stream() 失败计入 retry)
//   - 拿到 Stream 后 prefetch 第一帧探活:
//   - 第一帧返非 EOF 错误 → Close 流,把错误当作 Stream() 失败 retry
//   - 第一帧成功 / EOF → 用 prefixStream 包装,业务方 Recv 时先吐探活帧再继续 inner.Recv
//   - 由 fn 创建的 Stream 不受 PerCallTimeout 控制；StreamLLMToStep 在更外层
//     对一次完整流施加独立 timeout，避免 provider 内部重试重置总时限
//
// 注:streamCtx 等于外层 ctx,不带额外 timeout。fn 实现内部应该负责 connect timeout
// 等(走 HTTP client 配置)。
func StreamWithRetry(
	ctx context.Context,
	classifier ErrorClassifier,
	cfg *RetryConfig,
	fn func(streamCtx context.Context) (Stream, error),
) (Stream, error) {
	if classifier == nil {
		return nil, errors.New("loom.StreamWithRetry: classifier 不能为 nil")
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
		// 探活第一帧:Stream() 不返 error 不代表服务端真接受了请求 —
		// 部分 provider 在第一帧 chunk 才返 4xx/5xx body。
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

// classifyForBackoff 把原始错误翻成 backoff 期望的"是否 permanent"语义。
//
//   - Permanent / ctx.Canceled / ctx.DeadlineExceeded → 立即放弃(backoff.Permanent)
//   - RateLimit → 透传,不计数,由独立 elapsed budget 和 ctx 兜底
//   - Transient → 计数,超过 MaxRetries 升级为 Permanent;否则透传 retry
//   - Unknown → 按 Transient 处理
//
// nonRateLimitAttempts 是非 RateLimit 错误的累计次数(由调用方维护)。
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

// prefixStream 把 prefetch 的首帧挂在 inner stream 前。
//
// 业务方 Recv 时:
//  1. 首次 Recv → 返 prefetch 的首帧(或直接 EOF)
//  2. 之后 Recv → 透传到 inner.Recv
//
// Close 透传给 inner。
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
