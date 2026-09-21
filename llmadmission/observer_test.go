package llmadmission

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/loomagent/loom"
)

// An application exports the limiter's state through an Observer, which is otherwise invisible
// from outside, so the callbacks and what they carry are the contract under test here.
type recordingObserver struct {
	admissions []struct {
		quota  string
		waited time.Duration
		err    error
	}
	attempts []struct {
		quota, model, outcome string
		duration              time.Duration
	}
	states []struct {
		quota                         string
		globalInflight, quotaInflight int
		limit                         int
		cooldown                      time.Duration
	}
}

func (o *recordingObserver) ObserveAdmission(quota string, waited time.Duration, err error) {
	o.admissions = append(o.admissions, struct {
		quota  string
		waited time.Duration
		err    error
	}{quota, waited, err})
}

func (o *recordingObserver) ObserveAttempt(quota, model, outcome string, duration time.Duration) {
	o.attempts = append(o.attempts, struct {
		quota, model, outcome string
		duration              time.Duration
	}{quota, model, outcome, duration})
}

func (o *recordingObserver) SetQuotaState(quota string, globalInflight, quotaInflight, limit int, cooldown time.Duration) {
	o.states = append(o.states, struct {
		quota                         string
		globalInflight, quotaInflight int
		limit                         int
		cooldown                      time.Duration
	}{quota, globalInflight, quotaInflight, limit, cooldown})
}

func TestNewRejectsAnImpossibleWindow(t *testing.T) {
	valid := fixedConfig(4)
	tests := map[string]func(*Config){
		"no global limit":            func(c *Config) { c.GlobalLimit = 0 },
		"no quota minimum":           func(c *Config) { c.QuotaMin = 0 },
		"initial below minimum":      func(c *Config) { c.QuotaInitial = 0 },
		"maximum below initial":      func(c *Config) { c.QuotaMax = 1 },
		"maximum above global":       func(c *Config) { c.QuotaMax = 8 },
		"no increase interval":       func(c *Config) { c.IncreaseInterval = 0 },
		"no rate-limit cooldown":     func(c *Config) { c.RateLimitCooldown = 0 },
		"no unavailability cooldown": func(c *Config) { c.ServiceUnavailableCooldown = 0 },
	}
	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			config := valid
			break_(&config)
			if _, err := New(config, nil); err == nil {
				t.Fatal("an impossible window must be refused")
			}
		})
	}
	limiter, err := New(valid, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A nil observer is allowed and does nothing, rather than being a required argument.
	if _, err := limiter.Acquire(context.Background(), loom.AttemptMeta{QuotaKey: "k"}); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireRejectsWhatItCannotAdmit(t *testing.T) {
	limiter, err := New(fixedConfig(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Acquire(nil, loom.AttemptMeta{QuotaKey: "k"}); err == nil {
		t.Fatal("a nil context must be refused")
	}
	if _, err := limiter.Acquire(context.Background(), loom.AttemptMeta{}); err == nil ||
		!strings.Contains(err.Error(), "quota key is empty") {
		t.Fatalf("error = %v", err)
	}

	// A caller that has no label for its credential still gets a usable one, because an
	// observation with no label is worse than one labelled "unknown".
	observer := &recordingObserver{}
	limiter, err = New(fixedConfig(1), observer)
	if err != nil {
		t.Fatal(err)
	}
	permit, err := limiter.Acquire(context.Background(), loom.AttemptMeta{QuotaKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	permit.Finish(loom.AttemptResult{Success: true})
	if len(observer.attempts) != 1 || observer.attempts[0].quota != "unknown" || observer.attempts[0].model != "m" {
		t.Fatalf("attempts = %+v", observer.attempts)
	}
}

// What an application can export about one attempt: that it was admitted, how much of the
// window is in flight, and how it ended.
func TestObserverSeesAdmissionAndOutcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		observer := &recordingObserver{}
		limiter, err := New(fixedConfig(1), observer)
		if err != nil {
			t.Fatal(err)
		}
		permit, err := limiter.Acquire(context.Background(), loom.AttemptMeta{QuotaKey: "k", QuotaLabel: "vendor", Model: "pro"})
		if err != nil {
			t.Fatal(err)
		}
		permit.Finish(loom.AttemptResult{ErrorClass: loom.ErrorClassRateLimit, RetryAfter: 2 * time.Second})

		if len(observer.admissions) != 1 || observer.admissions[0].quota != "vendor" || observer.admissions[0].err != nil {
			t.Fatalf("admissions = %+v", observer.admissions)
		}
		if len(observer.attempts) != 1 || observer.attempts[0].outcome != "rate_limit" {
			t.Fatalf("attempts = %+v", observer.attempts)
		}
		last := observer.states[len(observer.states)-1]
		if last.quota != "vendor" || last.quotaInflight != 0 {
			t.Fatalf("state = %+v", last)
		}
		// A rate limit is followed by a cooldown, which is what the state reports.
		if last.cooldown <= 0 {
			t.Fatalf("cooldown = %s", last.cooldown)
		}

		// An admission that gives up reports why, so an application can see back-pressure
		// rather than only its absence.
		blocked, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := limiter.Acquire(blocked, loom.AttemptMeta{QuotaKey: "k", QuotaLabel: "vendor", Model: "pro"}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v", err)
		}
		lastAdmission := observer.admissions[len(observer.admissions)-1]
		if !errors.Is(lastAdmission.err, context.DeadlineExceeded) {
			t.Fatalf("admission = %+v", lastAdmission)
		}
	})
}

// The outcome is what an application graphs, so every ending has a name and an unrecognised
// class says so instead of being folded into another.
func TestAttemptOutcomeNames(t *testing.T) {
	for want, result := range map[string]loom.AttemptResult{
		"success":     {Success: true},
		"unavailable": {ServiceUnavailable: true},
		"unknown":     {ErrorClass: loom.ErrorClassUnknown},
		"transient":   {ErrorClass: loom.ErrorClassTransient},
		"rate_limit":  {ErrorClass: loom.ErrorClassRateLimit},
		"permanent":   {ErrorClass: loom.ErrorClassPermanent},
		"invalid":     {ErrorClass: loom.ErrorClass(99)},
	} {
		if got := attemptOutcome(result); got != want {
			t.Errorf("attemptOutcome(%+v) = %q, want %q", result, got, want)
		}
	}
	// A successful attempt that also carries an error class is still a success.
	if got := attemptOutcome(loom.AttemptResult{Success: true, ErrorClass: loom.ErrorClassPermanent}); got != "success" {
		t.Errorf("success with a class = %q", got)
	}
}

// Finishing twice is safe, and so is finishing a permit that was never issued.
func TestPermitFinishGuards(t *testing.T) {
	var absent *permit
	absent.Finish(loom.AttemptResult{Success: true})
	(&permit{}).Finish(loom.AttemptResult{Success: true})

	limiter, err := New(fixedConfig(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	permit, err := limiter.Acquire(context.Background(), loom.AttemptMeta{QuotaKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	permit.Finish(loom.AttemptResult{Success: true})
	permit.Finish(loom.AttemptResult{Success: true})
	// The window is free again, so the guard did not leak a slot.
	if _, err := limiter.Acquire(context.Background(), loom.AttemptMeta{QuotaKey: "k"}); err != nil {
		t.Fatalf("a finished permit left the window occupied: %v", err)
	}
}
