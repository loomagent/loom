package loom

import (
	"context"
	"errors"
	"io"
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

func TestChatWithRetry_RateLimitInfiniteUntilSuccess(t *testing.T) {
	attempts := 0
	rl := errors.New("429")
	c := mockClassifier{classify: func(err error) ErrorClass {
		if errors.Is(err, rl) {
			return ErrorClassRateLimit
		}
		return ErrorClassUnknown
	}}
	// MaxRetries=1 但 RateLimit 不计数,跑到第 10 次返回成功
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
