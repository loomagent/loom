package loom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// mockClassifier 测试用 classifier。
type mockClassifier struct {
	classify func(error) ErrorClass
}

func (m mockClassifier) ClassifyError(err error) ErrorClass {
	return m.classify(err)
}

// fastConfig 让单测跑得快(不等真 backoff)。
func fastConfig(maxRetries int) *RetryConfig {
	return &RetryConfig{
		MaxRetries:     maxRetries,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		PerCallTimeout: time.Second,
	}
}

func TestChatWithRetry_SuccessFirstAttempt(t *testing.T) {
	attempts := 0
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassUnknown }}
	resp, err := ChatWithRetry(context.Background(), c, fastConfig(2), func(context.Context) (*ChatResponse, error) {
		attempts++
		return &ChatResponse{Content: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
}

func TestChatWithRetry_TransientThenSuccess(t *testing.T) {
	attempts := 0
	transient := errors.New("transient")
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}
	resp, err := ChatWithRetry(context.Background(), c, fastConfig(3), func(context.Context) (*ChatResponse, error) {
		attempts++
		if attempts < 3 {
			return nil, transient
		}
		return &ChatResponse{Content: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
}

func TestChatWithRetry_TransientExhausted(t *testing.T) {
	attempts := 0
	sentinel := errors.New("never recovers")
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}
	_, err := ChatWithRetry(context.Background(), c, fastConfig(2), func(context.Context) (*ChatResponse, error) {
		attempts++
		return nil, sentinel
	})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err chain missing sentinel: %v", err)
	}
	// MaxRetries=2 → 第 3 次后判定 exhausted(nonRateLimitAttempts > 2),所以总尝试 3 次
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (1 initial + 2 retries)", attempts)
	}
}

func TestChatWithRetry_PermanentImmediateGiveUp(t *testing.T) {
	attempts := 0
	sentinel := errors.New("auth failed")
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassPermanent }}
	_, err := ChatWithRetry(context.Background(), c, fastConfig(5), func(context.Context) (*ChatResponse, error) {
		attempts++
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (permanent should give up immediately)", attempts)
	}
}

func TestChatWithRetry_RateLimitDoesNotConsumeTransientRetryCount(t *testing.T) {
	attempts := 0
	rl := errors.New("429")
	c := mockClassifier{classify: func(err error) ErrorClass {
		if errors.Is(err, rl) {
			return ErrorClassRateLimit
		}
		return ErrorClassUnknown
	}}
	// MaxRetries=1 但 RateLimit 不计次数,在 elapsed budget 内第 10 次返回成功。
	_, err := ChatWithRetry(context.Background(), c, fastConfig(1), func(context.Context) (*ChatResponse, error) {
		attempts++
		if attempts < 10 {
			return nil, rl
		}
		return &ChatResponse{Content: "finally"}, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if attempts != 10 {
		t.Errorf("attempts = %d, want 10 (RateLimit should not count toward MaxRetries)", attempts)
	}
}

func TestChatWithRetry_RateLimitCancelledByCtx(t *testing.T) {
	attempts := 0
	rl := errors.New("429")
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassRateLimit }}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := ChatWithRetry(ctx, c, fastConfig(1), func(context.Context) (*ChatResponse, error) {
		attempts++
		return nil, rl
	})
	if err == nil {
		t.Fatal("want error from ctx timeout, got nil")
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want >=2 (some retries before ctx timeout)", attempts)
	}
}

func TestChatWithRetry_RateLimitElapsedBudget(t *testing.T) {
	attempts := 0
	rl := errors.New("429")
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassRateLimit }}
	cfg := fastConfig(1)
	cfg.RateLimitMaxElapsed = 3 * time.Millisecond
	_, err := ChatWithRetry(context.Background(), c, cfg, func(context.Context) (*ChatResponse, error) {
		attempts++
		return nil, rl
	})
	if err == nil || !errors.Is(err, rl) {
		t.Fatalf("err = %v, want exhausted rate-limit error", err)
	}
	if attempts < 2 {
		t.Fatalf("attempts = %d, want retries before elapsed budget", attempts)
	}
}

func TestChatWithRetryAttemptPermitCoversOnlyPhysicalAttempt(t *testing.T) {
	limiter := &recordingAttemptLimiter{}
	cfg := fastConfig(2)
	cfg.AttemptLimiter = limiter
	cfg.AttemptMeta = AttemptMeta{QuotaKey: "key", QuotaLabel: "provider", Model: "model"}
	attempts := 0
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}
	_, err := ChatWithRetry(context.Background(), c, cfg, func(context.Context) (*ChatResponse, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("temporary")
		}
		return &ChatResponse{Content: "ok"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.active != 0 || limiter.maxActive != 1 || limiter.acquires != 2 || limiter.finishes != 2 {
		t.Fatalf("limiter state = %+v", limiter)
	}
}

func TestChatWithRetryPassesServiceUnavailableSignalToLimiter(t *testing.T) {
	limiter := &recordingAttemptLimiter{}
	cfg := fastConfig(1)
	cfg.Mode = RetryModeDisabled
	cfg.AttemptLimiter = limiter
	cfg.AttemptMeta = AttemptMeta{QuotaKey: "key", QuotaLabel: "provider", Model: "model"}
	sentinel := errors.New("503")
	_, err := ChatWithRetry(context.Background(), unavailableClassifier{}, cfg, func(context.Context) (*ChatResponse, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if len(limiter.results) != 1 || !limiter.results[0].ServiceUnavailable || limiter.results[0].ErrorClass != ErrorClassTransient {
		t.Fatalf("attempt results = %+v", limiter.results)
	}
}

func TestStreamWithRetryHoldsPermitUntilEOFAndCloseIsIdempotent(t *testing.T) {
	limiter := &recordingAttemptLimiter{}
	cfg := fastConfig(1)
	cfg.AttemptLimiter = limiter
	cfg.AttemptMeta = AttemptMeta{QuotaKey: "key", QuotaLabel: "provider", Model: "model"}
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}
	stream, err := StreamWithRetry(context.Background(), c, cfg, func(context.Context) (Stream, error) {
		return &fakeStream{chunks: []*Chunk{{ContentDelta: "hello"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if limiter.currentActive() != 1 {
		t.Fatalf("active after StreamWithRetry = %d, want 1", limiter.currentActive())
	}
	chunk, err := stream.Recv()
	if err != nil || chunk.ContentDelta != "hello" || limiter.currentActive() != 1 {
		t.Fatalf("first Recv = (%+v,%v), active=%d", chunk, err, limiter.currentActive())
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Recv err = %v, want EOF", err)
	}
	if limiter.currentActive() != 0 {
		t.Fatalf("active after EOF = %d, want 0", limiter.currentActive())
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.finishes != 1 || limiter.active != 0 {
		t.Fatalf("finish state after EOF+Close = %+v", limiter)
	}
}

func TestChatWithRetry_AttemptTimeoutRetriesWhileParentAlive(t *testing.T) {
	attempts := 0
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassPermanent }}
	cfg := &RetryConfig{
		Mode:           RetryModeUntilContext,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		PerCallTimeout: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	resp, err := ChatWithRetry(ctx, c, cfg, func(callCtx context.Context) (*ChatResponse, error) {
		attempts++
		if attempts == 1 {
			<-callCtx.Done()
			return nil, fmt.Errorf("decode response: %w", callCtx.Err())
		}
		return &ChatResponse{Content: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Content)
	}
}

func TestChatWithRetry_UntilContextIgnoresFiniteRetryCount(t *testing.T) {
	attempts := 0
	transient := errors.New("temporary upstream failure")
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}
	cfg := fastConfig(1)
	cfg.Mode = RetryModeUntilContext

	_, err := ChatWithRetry(context.Background(), c, cfg, func(context.Context) (*ChatResponse, error) {
		attempts++
		if attempts < 6 {
			return nil, transient
		}
		return &ChatResponse{Content: "recovered"}, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if attempts != 6 {
		t.Fatalf("attempts = %d, want 6", attempts)
	}
}

func TestChatWithRetry_ParentDeadlineStopsAttemptRetry(t *testing.T) {
	attempts := 0
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}
	cfg := &RetryConfig{
		Mode:           RetryModeUntilContext,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		PerCallTimeout: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	_, err := ChatWithRetry(ctx, c, cfg, func(callCtx context.Context) (*ChatResponse, error) {
		attempts++
		<-callCtx.Done()
		return nil, callCtx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want parent DeadlineExceeded", err)
	}
	if attempts < 2 {
		t.Fatalf("attempts = %d, want retries before parent deadline", attempts)
	}
}

func TestChatWithRetry_DisabledAttemptsOnce(t *testing.T) {
	attempts := 0
	transient := errors.New("temporary")
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}
	cfg := fastConfig(5)
	cfg.Mode = RetryModeDisabled

	_, err := ChatWithRetry(context.Background(), c, cfg, func(context.Context) (*ChatResponse, error) {
		attempts++
		return nil, transient
	})
	if !errors.Is(err, transient) {
		t.Fatalf("err = %v, want transient", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	class, ok := ErrorClassOf(err)
	if !ok || class != ErrorClassTransient {
		t.Fatalf("ErrorClassOf = (%v, %v), want transient,true", class, ok)
	}
}

func TestChatWithRetry_AttemptTimeoutReturnsTypedTransientWhenFiniteExhausted(t *testing.T) {
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassPermanent }}
	cfg := fastConfig(1)
	cfg.PerCallTimeout = time.Millisecond

	_, err := ChatWithRetry(context.Background(), c, cfg, func(callCtx context.Context) (*ChatResponse, error) {
		<-callCtx.Done()
		return nil, callCtx.Err()
	})
	if !errors.Is(err, ErrAttemptTimeout) {
		t.Fatalf("err = %v, want ErrAttemptTimeout", err)
	}
	class, ok := ErrorClassOf(err)
	if !ok || class != ErrorClassTransient {
		t.Fatalf("ErrorClassOf = (%v, %v), want transient,true", class, ok)
	}
}

func TestStreamWithRetry_PrefetchSuccess(t *testing.T) {
	attempts := 0
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassUnknown }}
	stream, err := StreamWithRetry(context.Background(), c, fastConfig(2), func(context.Context) (Stream, error) {
		attempts++
		return &fakeStream{chunks: []*Chunk{{ContentDelta: "hello"}, {ContentDelta: "world"}}}, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	// 验证 prefetch 首帧能拿到
	c1, _ := stream.Recv()
	if c1 == nil || c1.ContentDelta != "hello" {
		t.Errorf("first chunk = %+v, want hello", c1)
	}
	c2, _ := stream.Recv()
	if c2 == nil || c2.ContentDelta != "world" {
		t.Errorf("second chunk = %+v, want world", c2)
	}
	_, err = stream.Recv()
	if !errors.Is(err, io.EOF) {
		t.Errorf("third recv err = %v, want EOF", err)
	}
}

func TestStreamWithRetry_TransientThenSuccess(t *testing.T) {
	attempts := 0
	transient := errors.New("transient")
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}
	stream, err := StreamWithRetry(context.Background(), c, fastConfig(3), func(context.Context) (Stream, error) {
		attempts++
		if attempts < 3 {
			return nil, transient
		}
		return &fakeStream{chunks: []*Chunk{{ContentDelta: "ok"}}}, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	first, _ := stream.Recv()
	if first == nil {
		t.Fatalf("first Recv returned nil chunk")
	}
	if first.ContentDelta != "ok" {
		t.Errorf("first = %+v", first)
	}
}

func TestStreamWithRetry_FirstFrameFailRetries(t *testing.T) {
	attempts := 0
	transient := errors.New("first frame fail")
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}
	stream, err := StreamWithRetry(context.Background(), c, fastConfig(3), func(context.Context) (Stream, error) {
		attempts++
		if attempts < 3 {
			// stream 创建成功,但首帧失败
			return &fakeStream{firstErr: transient}, nil
		}
		return &fakeStream{chunks: []*Chunk{{ContentDelta: "good"}}}, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	first, _ := stream.Recv()
	if first == nil {
		t.Fatalf("first Recv returned nil chunk")
	}
	if first.ContentDelta != "good" {
		t.Errorf("first = %+v", first)
	}
}

func TestStreamWithRetry_MidStreamFailNotRetried(t *testing.T) {
	attempts := 0
	transient := errors.New("mid-stream fail")
	c := mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}
	stream, err := StreamWithRetry(context.Background(), c, fastConfig(3), func(context.Context) (Stream, error) {
		attempts++
		return &fakeStream{
			chunks:   []*Chunk{{ContentDelta: "first"}},
			midErrAt: 1, // 拿完 first 后 Recv 返 transient,不再 retry
			midErr:   transient,
		}, nil
	})
	if err != nil {
		t.Fatalf("initial stream err = %v", err)
	}
	if attempts != 1 {
		t.Errorf("initial attempts = %d, want 1", attempts)
	}
	first, _ := stream.Recv()
	if first == nil {
		t.Fatalf("first Recv returned nil chunk")
	}
	if first.ContentDelta != "first" {
		t.Errorf("first = %+v", first)
	}
	// 第二次 Recv 应该透传 transient err,不 retry
	_, err = stream.Recv()
	if !errors.Is(err, transient) {
		t.Errorf("mid-stream err = %v, want transient (no retry)", err)
	}
}

// fakeStream 简单的 Stream 实现,支持注入 firstErr / midErrAt。
type fakeStream struct {
	chunks   []*Chunk
	idx      int
	firstErr error // 非 nil 时,第一次 Recv 直接返此 err
	midErrAt int   // 拿完前 midErrAt 帧后,下一次 Recv 返 midErr
	midErr   error
	closed   bool
}

func (s *fakeStream) Recv() (*Chunk, error) {
	if s.firstErr != nil && s.idx == 0 {
		return nil, s.firstErr
	}
	if s.midErr != nil && s.idx >= s.midErrAt {
		return nil, s.midErr
	}
	if s.idx >= len(s.chunks) {
		return nil, io.EOF
	}
	c := s.chunks[s.idx]
	s.idx++
	return c, nil
}

func (s *fakeStream) Close() error {
	s.closed = true
	return nil
}

type recordingAttemptLimiter struct {
	mu        sync.Mutex
	active    int
	maxActive int
	acquires  int
	finishes  int
	results   []AttemptResult
}

func (l *recordingAttemptLimiter) Acquire(context.Context, AttemptMeta) (AttemptPermit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active++
	l.acquires++
	l.maxActive = max(l.maxActive, l.active)
	return &recordingAttemptPermit{limiter: l}, nil
}

func (l *recordingAttemptLimiter) currentActive() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

type recordingAttemptPermit struct {
	once    sync.Once
	limiter *recordingAttemptLimiter
}

func (p *recordingAttemptPermit) Finish(result AttemptResult) {
	p.once.Do(func() {
		p.limiter.mu.Lock()
		defer p.limiter.mu.Unlock()
		p.limiter.active--
		p.limiter.finishes++
		p.limiter.results = append(p.limiter.results, result)
	})
}

type unavailableClassifier struct{}

func (unavailableClassifier) ClassifyError(error) ErrorClass  { return ErrorClassTransient }
func (unavailableClassifier) IsServiceUnavailable(error) bool { return true }
