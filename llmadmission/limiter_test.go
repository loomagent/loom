package llmadmission

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/loomagent/loom"
)

func fixedConfig(limit int) Config {
	return Config{
		GlobalLimit: limit, QuotaMin: limit, QuotaInitial: limit, QuotaMax: limit,
		IncreaseInterval: time.Hour, RateLimitCooldown: time.Millisecond, ServiceUnavailableCooldown: time.Millisecond,
	}
}

func TestLimiterSharesCredentialWindowAcrossModels(t *testing.T) {
	limiter, err := New(fixedConfig(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := limiter.Acquire(context.Background(), loom.AttemptMeta{QuotaKey: "same-key", QuotaLabel: "deepseek", Model: "pro"})
	if err != nil {
		t.Fatal(err)
	}

	blockedCtx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err = limiter.Acquire(blockedCtx, loom.AttemptMeta{QuotaKey: "same-key", QuotaLabel: "deepseek", Model: "flash"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second model acquire error = %v, want deadline", err)
	}

	first.Finish(loom.AttemptResult{Success: true})
	second, err := limiter.Acquire(context.Background(), loom.AttemptMeta{QuotaKey: "same-key", QuotaLabel: "deepseek", Model: "flash"})
	if err != nil {
		t.Fatal(err)
	}
	second.Finish(loom.AttemptResult{Success: true})
}

func TestLimiterRateLimitHalvesWindowAndAppliesSharedCooldown(t *testing.T) {
	limiter, err := New(Config{
		GlobalLimit: 8, QuotaMin: 2, QuotaInitial: 4, QuotaMax: 8,
		IncreaseInterval: time.Hour, RateLimitCooldown: 40 * time.Millisecond, ServiceUnavailableCooldown: time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	meta := loom.AttemptMeta{QuotaKey: "key", QuotaLabel: "deepseek", Model: "pro"}
	permits := make([]loom.AttemptPermit, 4)
	for i := range permits {
		permits[i], err = limiter.Acquire(context.Background(), meta)
		if err != nil {
			t.Fatal(err)
		}
	}
	permits[0].Finish(loom.AttemptResult{ErrorClass: loom.ErrorClassRateLimit})
	for _, permit := range permits[1:] {
		permit.Finish(loom.AttemptResult{Success: true})
	}

	limiter.mu.Lock()
	limit := limiter.quotas[meta.QuotaKey].limit
	limiter.mu.Unlock()
	if limit != 2 {
		t.Fatalf("quota limit = %d, want 2", limit)
	}
	blockedCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := limiter.Acquire(blockedCtx, meta); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cooldown acquire error = %v, want deadline", err)
	}

	time.Sleep(45 * time.Millisecond)
	first, err := limiter.Acquire(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	second, err := limiter.Acquire(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	thirdCtx, thirdCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer thirdCancel()
	if _, err := limiter.Acquire(thirdCtx, meta); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third acquire error = %v, want quota-window deadline", err)
	}
	first.Finish(loom.AttemptResult{Success: true})
	second.Finish(loom.AttemptResult{Success: true})
}

func TestLimiterHealthyTrafficAdditivelyIncreasesWithinHardMax(t *testing.T) {
	limiter, err := New(Config{
		GlobalLimit: 3, QuotaMin: 1, QuotaInitial: 1, QuotaMax: 2,
		IncreaseInterval: time.Millisecond, RateLimitCooldown: time.Millisecond, ServiceUnavailableCooldown: time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	meta := loom.AttemptMeta{QuotaKey: "key", QuotaLabel: "deepseek", Model: "pro"}
	permit, err := limiter.Acquire(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	permit.Finish(loom.AttemptResult{Success: true})

	limiter.mu.Lock()
	limit := limiter.quotas[meta.QuotaKey].limit
	limiter.mu.Unlock()
	if limit != 2 {
		t.Fatalf("quota limit = %d, want additive increase to 2", limit)
	}
}

func TestLimiterServiceUnavailableUsesSharedHalfOpenCircuitWithoutReducingQuota(t *testing.T) {
	limiter, err := New(Config{
		GlobalLimit: 8, QuotaMin: 2, QuotaInitial: 4, QuotaMax: 8,
		IncreaseInterval: time.Hour, RateLimitCooldown: time.Millisecond, ServiceUnavailableCooldown: 30 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	meta := loom.AttemptMeta{QuotaKey: "key", QuotaLabel: "deepseek", Model: "pro"}
	first, err := limiter.Acquire(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	oldInflight, err := limiter.Acquire(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	first.Finish(loom.AttemptResult{ErrorClass: loom.ErrorClassTransient, ServiceUnavailable: true})
	oldInflight.Finish(loom.AttemptResult{Success: true})

	limiter.mu.Lock()
	limit := limiter.quotas[meta.QuotaKey].limit
	limiter.mu.Unlock()
	if limit != 4 {
		t.Fatalf("503 changed quota limit to %d, want 4", limit)
	}
	blockedCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := limiter.Acquire(blockedCtx, meta); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("circuit cooldown acquire error = %v, want deadline", err)
	}

	time.Sleep(35 * time.Millisecond)
	probe, err := limiter.Acquire(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer secondCancel()
	if _, err := limiter.Acquire(secondCtx, meta); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parallel half-open acquire error = %v, want deadline", err)
	}
	probe.Finish(loom.AttemptResult{Success: true})
	afterProbe, err := limiter.Acquire(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	afterProbe.Finish(loom.AttemptResult{Success: true})
}

func TestPermitFinishIsIdempotent(t *testing.T) {
	limiter, err := New(fixedConfig(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	meta := loom.AttemptMeta{QuotaKey: "key", QuotaLabel: "deepseek"}
	permit, err := limiter.Acquire(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	permit.Finish(loom.AttemptResult{Success: true})
	permit.Finish(loom.AttemptResult{Success: true})
	limiter.mu.Lock()
	global, quota := limiter.globalInflight, limiter.quotas[meta.QuotaKey].inflight
	limiter.mu.Unlock()
	if global != 0 || quota != 0 {
		t.Fatalf("inflight after duplicate finish = global:%d quota:%d", global, quota)
	}
}
