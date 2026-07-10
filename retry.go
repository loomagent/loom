package loom

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// ErrorClassRateLimit 限流,无限 retry(不计入 MaxRetries),由 ctx 兜底总时长。
	// 典型:HTTP 429、provider 自定的 RateLimit 状态。
	ErrorClassRateLimit
	// ErrorClassPermanent 立即放弃,不 retry。
	// 典型:401/402/403(auth / 余额不足)、400(bad request)、内容审核拒绝、
	// 模型不存在;以及 ctx.Canceled / ctx.DeadlineExceeded(外层主动终止)。
	ErrorClassPermanent
)

// ErrorClassifier 把 provider 原生 error 翻译成 ErrorClass。
//
// 每个 provider 必须实现一份(没默认值 — 默认值会让真错误被误识别成 Transient
// 然后无限 retry)。Provider 实现一般是:
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

// RetryConfig 通用 retry 调度配置。所有 provider 共享。
//
// 字段为零值时,DefaultRetryConfig 的默认值会在 ChatWithRetry / StreamWithRetry
// 入口生效。
type RetryConfig struct {
	// MaxRetries Transient 类错误的最大重试次数(首次调用不计)。
	// 例 MaxRetries=2 表示最多尝试 3 次(1 次首调 + 2 次重试)。
	// RateLimit 类不受此限制,Permanent 类完全跳过。
	MaxRetries int

	// InitialBackoff 首次 backoff 间隔。指数退避从此起步。
	InitialBackoff time.Duration

	// MaxBackoff backoff 上限。指数退避不超过此值。
	MaxBackoff time.Duration

	// PerCallTimeout 每次单次 Chat 调用的 ctx 超时。
	// 仅 ChatWithRetry 使用 — Stream 不能 wrap 单调用 timeout(stream 全程可能
	// 跨几分钟,中途 timeout 会把活着的 stream 打断)。Stream 卡死靠 HTTP client
	// ReadTimeout 兜底,由 provider 实现自己配置。
	PerCallTimeout time.Duration
}

// DefaultRetryConfig 返回框架推荐默认值。
//
// 业务方一般不需要改这些值 — 这是基于"大多数 SaaS LLM 限流 / 抖动模式"的折中。
// 真要调,例如 dev 环境想快速 fail 改 MaxRetries=0,或长 prompt 给更长 PerCallTimeout。
func DefaultRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxRetries:     2,
		InitialBackoff: time.Second,
		MaxBackoff:     30 * time.Second,
		PerCallTimeout: 5 * time.Minute,
	}
}

// applyDefaults 对零值字段填默认值(就地修改)。
func (c *RetryConfig) applyDefaults() {
	def := DefaultRetryConfig()
	if c.MaxRetries == 0 {
		c.MaxRetries = def.MaxRetries
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
//   - 错误分类:Transient 计数 retry / RateLimit 不计数无限 retry / Permanent 立即放弃
//   - 外层 ctx 取消立即放弃(把 ctx.Err 当 Permanent)
//
// classifier 不可为 nil — 框架不提供默认分类器,因为"默认 Transient"会让真错误无限 retry。
func ChatWithRetry(
	ctx context.Context,
	classifier ErrorClassifier,
	cfg *RetryConfig,
	fn func(callCtx context.Context) (*ChatResponse, error),
) (*ChatResponse, error) {
	if classifier == nil {
		return nil, errors.New("loom.ChatWithRetry: classifier 不能为 nil")
	}
	if cfg == nil {
		cfg = DefaultRetryConfig()
	} else {
		cfg.applyDefaults()
	}

	nonRateLimitAttempts := 0
	return backoff.Retry(ctx, func() (*ChatResponse, error) {
		callCtx, cancel := context.WithTimeout(ctx, cfg.PerCallTimeout)
		defer cancel()
		resp, err := fn(callCtx)
		if err != nil {
			return nil, classifyForBackoff(err, classifier, cfg, &nonRateLimitAttempts)
		}
		return resp, nil
	},
		backoff.WithBackOff(cfg.newBackoff()),
	)
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
//   - 由 fn 创建的 Stream 不受 PerCallTimeout 控制 — stream 生命周期太长不适合 wrap 单次 timeout
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
	if cfg == nil {
		cfg = DefaultRetryConfig()
	} else {
		cfg.applyDefaults()
	}

	nonRateLimitAttempts := 0
	return backoff.Retry(ctx, func() (Stream, error) {
		stream, err := fn(ctx)
		if err != nil {
			return nil, classifyForBackoff(err, classifier, cfg, &nonRateLimitAttempts)
		}
		// 探活第一帧:Stream() 不返 error 不代表服务端真接受了请求 —
		// 部分 provider 在第一帧 chunk 才返 4xx/5xx body。
		first, recvErr := stream.Recv()
		if recvErr != nil && !errors.Is(recvErr, io.EOF) {
			_ = stream.Close()
			return nil, classifyForBackoff(recvErr, classifier, cfg, &nonRateLimitAttempts)
		}
		return &prefixStream{inner: stream, first: first, firstEOF: errors.Is(recvErr, io.EOF)}, nil
	},
		backoff.WithBackOff(cfg.newBackoff()),
	)
}

// classifyForBackoff 把原始错误翻成 backoff 期望的"是否 permanent"语义。
//
//   - Permanent / ctx.Canceled / ctx.DeadlineExceeded → 立即放弃(backoff.Permanent)
//   - RateLimit → 透传,不计数,backoff 继续 retry(等价于无限 retry,由 ctx 兜底)
//   - Transient → 计数,超过 MaxRetries 升级为 Permanent;否则透传 retry
//   - Unknown → 按 Transient 处理
//
// nonRateLimitAttempts 是非 RateLimit 错误的累计次数(由调用方维护)。
func classifyForBackoff(err error, classifier ErrorClassifier, cfg *RetryConfig, nonRateLimitAttempts *int) error {
	// ctx 错误优先 — 外层 cancel 必须立即停,不论 classifier 怎么判定
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return backoff.Permanent(err)
	}
	class := classifier.ClassifyError(err)
	switch class {
	case ErrorClassPermanent:
		return backoff.Permanent(err)
	case ErrorClassRateLimit:
		// 不计数,backoff 继续 retry(无限)
		return err
	case ErrorClassTransient, ErrorClassUnknown:
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
}

func (s *prefixStream) Recv() (*Chunk, error) {
	if !s.firstConsumed {
		s.firstConsumed = true
		if s.firstEOF {
			return nil, io.EOF
		}
		return s.first, nil
	}
	return s.inner.Recv()
}

func (s *prefixStream) Close() error {
	return s.inner.Close()
}
