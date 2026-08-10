// Package llmadmission implements bounded, credential-scoped admission for
// physical LLM request attempts.
package llmadmission

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/loomagent/loom"
)

type Config struct {
	GlobalLimit                int
	QuotaMin                   int
	QuotaInitial               int
	QuotaMax                   int
	IncreaseInterval           time.Duration
	RateLimitCooldown          time.Duration
	ServiceUnavailableCooldown time.Duration
}

// Observer receives low-cardinality, non-secret state. SetQuotaState is called
// while Limiter serializes state changes, so implementations must return
// quickly and must not call back into Limiter.
type Observer interface {
	ObserveAdmission(quota string, waited time.Duration, err error)
	ObserveAttempt(quota, model, outcome string, duration time.Duration)
	SetQuotaState(quota string, globalInflight, quotaInflight, quotaLimit int, cooldown time.Duration)
}

type noopObserver struct{}

func (noopObserver) ObserveAdmission(string, time.Duration, error)        {}
func (noopObserver) ObserveAttempt(string, string, string, time.Duration) {}
func (noopObserver) SetQuotaState(string, int, int, int, time.Duration)   {}

type quotaState struct {
	label                string
	inflight             int
	limit                int
	successesSinceChange int
	lastChange           time.Time
	cooldownUntil        time.Time
	serviceOpen          bool
	serviceProbeInFlight bool
	serviceCooldownUntil time.Time
}

// Limiter owns one process-wide global window and one bounded AIMD window per
// credential-derived quota key.
type Limiter struct {
	mu             sync.Mutex
	config         Config
	observer       Observer
	globalInflight int
	quotas         map[string]*quotaState
	notify         chan struct{}
}

var _ loom.AttemptLimiter = (*Limiter)(nil)

func New(config Config, observer Observer) (*Limiter, error) {
	if config.GlobalLimit < 1 || config.QuotaMin < 1 ||
		config.QuotaInitial < config.QuotaMin || config.QuotaMax < config.QuotaInitial ||
		config.QuotaMax > config.GlobalLimit {
		return nil, errors.New("llm admission: require 1 <= quota min <= initial <= max <= global")
	}
	if config.IncreaseInterval <= 0 || config.RateLimitCooldown <= 0 || config.ServiceUnavailableCooldown <= 0 {
		return nil, errors.New("llm admission: timing values must be positive")
	}
	if observer == nil {
		observer = noopObserver{}
	}
	return &Limiter{
		config: config, observer: observer,
		quotas: make(map[string]*quotaState), notify: make(chan struct{}),
	}, nil
}

func (l *Limiter) Acquire(ctx context.Context, meta loom.AttemptMeta) (loom.AttemptPermit, error) {
	if ctx == nil {
		return nil, errors.New("llm admission: ctx is nil")
	}
	if meta.QuotaKey == "" {
		return nil, errors.New("llm admission: quota key is empty")
	}
	if meta.QuotaLabel == "" {
		meta.QuotaLabel = "unknown"
	}
	started := time.Now()
	for {
		now := time.Now()
		l.mu.Lock()
		quota := l.quotaLocked(meta, now)
		serviceReady, serviceProbe := serviceAdmission(quota, now)
		if !now.Before(quota.cooldownUntil) && serviceReady &&
			l.globalInflight < l.config.GlobalLimit && quota.inflight < quota.limit {
			l.globalInflight++
			quota.inflight++
			if serviceProbe {
				quota.serviceProbeInFlight = true
			}
			globalInflight, quotaInflight, quotaLimit := l.globalInflight, quota.inflight, quota.limit
			l.observer.SetQuotaState(quota.label, globalInflight, quotaInflight, quotaLimit, 0)
			l.mu.Unlock()
			l.observer.ObserveAdmission(quota.label, time.Since(started), nil)
			return &permit{limiter: l, quotaKey: meta.QuotaKey, meta: meta, started: time.Now(), serviceProbe: serviceProbe}, nil
		}
		notify := l.notify
		cooldownUntil := quota.cooldownUntil
		if quota.serviceOpen && quota.serviceCooldownUntil.After(cooldownUntil) {
			cooldownUntil = quota.serviceCooldownUntil
		}
		cooldownWait := max(time.Duration(0), time.Until(cooldownUntil))
		l.mu.Unlock()

		var timer *time.Timer
		var timerC <-chan time.Time
		if cooldownWait > 0 {
			timer = time.NewTimer(cooldownWait)
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			l.observer.ObserveAdmission(meta.QuotaLabel, time.Since(started), ctx.Err())
			return nil, ctx.Err()
		case <-notify:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
		}
	}
}

func (l *Limiter) quotaLocked(meta loom.AttemptMeta, now time.Time) *quotaState {
	quota := l.quotas[meta.QuotaKey]
	if quota != nil {
		return quota
	}
	quota = &quotaState{label: meta.QuotaLabel, limit: l.config.QuotaInitial, lastChange: now}
	l.quotas[meta.QuotaKey] = quota
	return quota
}

func serviceAdmission(quota *quotaState, now time.Time) (ready bool, probe bool) {
	if !quota.serviceOpen {
		return true, false
	}
	if now.Before(quota.serviceCooldownUntil) || quota.serviceProbeInFlight {
		return false, false
	}
	return true, true
}

func (l *Limiter) finish(quotaKey string, meta loom.AttemptMeta, started time.Time, serviceProbe bool, result loom.AttemptResult) {
	now := time.Now()
	outcome := attemptOutcome(result)
	l.mu.Lock()
	quota := l.quotas[quotaKey]
	if quota == nil || quota.inflight == 0 || l.globalInflight == 0 {
		l.mu.Unlock()
		return
	}
	quota.inflight--
	l.globalInflight--
	if serviceProbe {
		quota.serviceProbeInFlight = false
	}
	if result.ServiceUnavailable {
		quota.serviceOpen = true
		quota.serviceProbeInFlight = false
		quota.serviceCooldownUntil = now.Add(l.config.ServiceUnavailableCooldown)
		quota.successesSinceChange = 0
	} else if serviceProbe {
		if result.Success || result.ErrorClass == loom.ErrorClassRateLimit || result.ErrorClass == loom.ErrorClassPermanent {
			quota.serviceOpen = false
			quota.serviceCooldownUntil = time.Time{}
		} else {
			quota.serviceOpen = true
			quota.serviceCooldownUntil = now.Add(l.config.ServiceUnavailableCooldown)
		}
	}
	if result.Success && !quota.serviceOpen {
		quota.successesSinceChange++
		if quota.limit < l.config.QuotaMax &&
			quota.successesSinceChange >= quota.limit &&
			now.Sub(quota.lastChange) >= l.config.IncreaseInterval {
			quota.limit++
			quota.successesSinceChange = 0
			quota.lastChange = now
		}
	} else if result.ErrorClass == loom.ErrorClassRateLimit {
		quota.limit = max(l.config.QuotaMin, quota.limit/2)
		quota.successesSinceChange = 0
		quota.lastChange = now
		cooldown := max(l.config.RateLimitCooldown, result.RetryAfter)
		candidate := now.Add(cooldown)
		if candidate.After(quota.cooldownUntil) {
			quota.cooldownUntil = candidate
		}
	}
	l.broadcastLocked()
	globalInflight, quotaInflight, quotaLimit := l.globalInflight, quota.inflight, quota.limit
	cooldownUntil := quota.cooldownUntil
	if quota.serviceOpen && quota.serviceCooldownUntil.After(cooldownUntil) {
		cooldownUntil = quota.serviceCooldownUntil
	}
	cooldown := max(time.Duration(0), time.Until(cooldownUntil))
	label := quota.label
	l.observer.SetQuotaState(label, globalInflight, quotaInflight, quotaLimit, cooldown)
	l.mu.Unlock()

	l.observer.ObserveAttempt(label, meta.Model, outcome, time.Since(started))
}

func (l *Limiter) broadcastLocked() {
	close(l.notify)
	l.notify = make(chan struct{})
}

func attemptOutcome(result loom.AttemptResult) string {
	if result.Success {
		return "success"
	}
	if result.ServiceUnavailable {
		return "unavailable"
	}
	switch result.ErrorClass {
	case loom.ErrorClassUnknown:
		return "unknown"
	case loom.ErrorClassTransient:
		return "transient"
	case loom.ErrorClassRateLimit:
		return "rate_limit"
	case loom.ErrorClassPermanent:
		return "permanent"
	default:
		return "invalid"
	}
}

type permit struct {
	once         sync.Once
	limiter      *Limiter
	quotaKey     string
	meta         loom.AttemptMeta
	started      time.Time
	serviceProbe bool
}

func (p *permit) Finish(result loom.AttemptResult) {
	if p == nil || p.limiter == nil {
		return
	}
	p.once.Do(func() {
		p.limiter.finish(p.quotaKey, p.meta, p.started, p.serviceProbe, result)
	})
}
