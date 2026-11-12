package loom

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Invocation identifies one selected model under a logical call. Failover keeps
// CallID and allocates another InvocationID; physical retries get AttemptIDs.
type Invocation struct {
	CallID       string    `json:"call_id"`
	InvocationID string    `json:"invocation_id"`
	Purpose      string    `json:"purpose,omitempty"`
	ModelInfo    ModelInfo `json:"model_info"`
	Streaming    bool      `json:"streaming"`
}

// LifecycleEvent is metadata-only. Err is delivered to trusted host code for
// classification but is never serialized. It must not be persisted verbatim.
type LifecycleEvent struct {
	// Request is available synchronously on call_started when supplied by the
	// caller. It is read-only and MUST NOT be retained or persisted as raw content.
	// It is excluded from serialization regardless of content-capture settings.
	Request               *ChatRequest `json:"-"`
	ID                    string       `json:"id"`
	Kind                  string       `json:"kind"`
	Invocation            Invocation   `json:"invocation"`
	AttemptID             string       `json:"attempt_id,omitempty"`
	Time                  time.Time    `json:"time"`
	Outcome               string       `json:"outcome,omitempty"`
	ErrorClass            ErrorClass   `json:"error_class,omitzero"`
	ProviderReportedModel string       `json:"provider_reported_model,omitempty"`
	FinishReason          FinishReason `json:"finish_reason,omitempty"`
	Usage                 *Usage       `json:"usage,omitempty"`
	Err                   error        `json:"-"`
}

type observationConfig struct{ request *ChatRequest }

// ObservationOption adds transient evidence to a lifecycle observation.
type ObservationOption func(*observationConfig)

// WithObservedRequest exposes the selected helper-level request to the trusted
// observer before model execution. It is not the provider's final HTTP payload.
func WithObservedRequest(request ChatRequest) ObservationOption {
	return func(cfg *observationConfig) { cfg.request = &request }
}

// LifecycleObserver persists metadata before requests and before success is
// returned. Record must be concurrent-safe. A returned error stops the call;
// callers must not fail over or retry an audit failure as a provider failure.
type LifecycleObserver interface {
	Record(context.Context, LifecycleEvent) error
}
type LifecycleObserverFunc func(context.Context, LifecycleEvent) error

func (f LifecycleObserverFunc) Record(ctx context.Context, event LifecycleEvent) error {
	return f(ctx, event)
}

// ObservationError denotes mandatory lifecycle persistence failure.
type ObservationError struct{ Err error }

func (e *ObservationError) Error() string {
	return fmt.Sprintf("loom: lifecycle persistence: %v", e.Err)
}
func (e *ObservationError) Unwrap() error { return e.Err }

type observerKey struct{}
type invocationKey struct{}
type callKey struct{}
type attemptKey struct{}
type callScope struct{ id, purpose string }
type invocationState struct {
	value    Invocation
	mu       sync.Mutex
	observer LifecycleObserver
	active   bool
	failure  error
}

func WithLifecycleObserver(ctx context.Context, observer LifecycleObserver) context.Context {
	return context.WithValue(ctx, observerKey{}, observer)
}

// WithCallScope starts a distinct logical call. Hosts calling Chat/Stream
// directly can use it to set purpose; the standard helpers do so themselves.
func WithCallScope(ctx context.Context, purpose string) context.Context {
	ctx = context.WithValue(ctx, callKey{}, callScope{id: "call_" + rand.Text(), purpose: purpose})
	return context.WithValue(ctx, invocationKey{}, (*invocationState)(nil))
}

func prepareInvocation(ctx context.Context, info ModelInfo, streaming bool) context.Context {
	if state, _ := ctx.Value(invocationKey{}).(*invocationState); state != nil && state.value.ModelInfo == info {
		state.mu.Lock()
		reuse := state.active || state.value.Streaming == streaming
		state.mu.Unlock()
		if reuse {
			return ctx
		}
	}
	scope, _ := ctx.Value(callKey{}).(callScope)
	if scope.id == "" {
		ctx = WithCallScope(ctx, "")
		scope = ctx.Value(callKey{}).(callScope)
	}
	return context.WithValue(ctx, invocationKey{}, &invocationState{value: Invocation{CallID: scope.id, InvocationID: "invoke_" + rand.Text(), Purpose: scope.purpose, ModelInfo: info, Streaming: streaming}})
}

func InvocationFromContext(ctx context.Context) Invocation {
	if state, _ := ctx.Value(invocationKey{}).(*invocationState); state != nil {
		return state.value
	}
	return Invocation{}
}
func AttemptIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(attemptKey{}).(string)
	return id
}
func newAttemptContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, attemptKey{}, "attempt_"+rand.Text())
}

func recordLifecycle(ctx context.Context, event LifecycleEvent) error {
	state, _ := ctx.Value(invocationKey{}).(*invocationState)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	observer := state.observer
	state.mu.Unlock()
	if observer == nil {
		return nil
	}
	if err := observer.Record(ctx, event); err != nil {
		failure := &ObservationError{Err: err}
		state.mu.Lock()
		state.failure = errors.Join(state.failure, failure)
		state.mu.Unlock()
		return failure
	}
	return nil
}
func lifecycleFailure(ctx context.Context) error {
	state, _ := ctx.Value(invocationKey{}).(*invocationState)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.failure
}
func beginInvocation(ctx context.Context, info ModelInfo, streaming bool, opts ...ObservationOption) (context.Context, bool, error) {
	ctx = prepareInvocation(ctx, info, streaming)
	state := ctx.Value(invocationKey{}).(*invocationState)
	observer, _ := ctx.Value(observerKey{}).(LifecycleObserver)
	state.mu.Lock()
	if state.active || observer == nil {
		state.mu.Unlock()
		return ctx, false, nil
	}
	state.active = true
	state.observer = observer
	state.mu.Unlock()
	var cfg observationConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	event := LifecycleEvent{ID: state.value.InvocationID + ":start", Kind: "call_started", Invocation: state.value, Time: time.Now().UTC()}
	event.Request = cfg.request
	return ctx, true, recordLifecycle(ctx, event)
}
func endInvocation(ctx context.Context, response *ChatResponse, outcome string, err error) error {
	inv := InvocationFromContext(ctx)
	event := LifecycleEvent{ID: inv.InvocationID + ":end", Kind: "call_finished", Invocation: inv, Time: time.Now().UTC(), Outcome: outcome, Err: err}
	if class, ok := ErrorClassOf(err); ok {
		event.ErrorClass = class
	}
	if response != nil {
		event.ProviderReportedModel = response.Model
		event.FinishReason = response.FinishReason
		if nonZeroUsage(response.Usage) {
			usage := response.Usage
			event.Usage = &usage
		}
	}
	return recordLifecycle(ctx, event)
}
func callOutcome(err error) string {
	if err == nil {
		return "completed"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return "failed"
}

// ObserveChat instruments a selected model. Nested wrappers reuse the same
// invocation; a missing observer leaves provider behavior unchanged.
func ObserveChat(ctx context.Context, info ModelInfo, call func(context.Context) (*ChatResponse, error), opts ...ObservationOption) (*ChatResponse, error) {
	ctx, owned, err := beginInvocation(ctx, info, false, opts...)
	if err != nil {
		return nil, err
	}
	ctx, integrity := beginTextIntegrity(ctx)
	response, err := call(ctx)
	if integrity != nil {
		integrity.response(response, err != nil)
	}
	if !owned {
		return response, err
	}
	err = errors.Join(err, lifecycleFailure(ctx))
	finishErr := endInvocation(ctx, response, callOutcome(err), err)
	return response, errors.Join(err, finishErr)
}

// ObserveStream records terminal consumption or early close exactly once.
// A failed terminal record replaces EOF so a host cannot commit a false success.
func ObserveStream(ctx context.Context, info ModelInfo, call func(context.Context) (Stream, error), opts ...ObservationOption) (Stream, error) {
	ctx, owned, err := beginInvocation(ctx, info, true, opts...)
	if err != nil {
		return nil, err
	}
	ctx, integrity := beginTextIntegrity(ctx)
	stream, err := call(ctx)
	wrapIntegrity := func(stream Stream) Stream {
		if integrity != nil && stream != nil {
			return &textIntegrityStream{Stream: stream, capture: integrity}
		}
		return stream
	}
	if !owned {
		return wrapIntegrity(stream), err
	}
	err = errors.Join(err, lifecycleFailure(ctx))
	if err != nil || stream == nil {
		if err == nil {
			err = errors.New("loom: model returned nil stream")
		}
		if stream != nil {
			_ = stream.Close()
		}
		return nil, errors.Join(err, endInvocation(ctx, nil, callOutcome(err), err))
	}
	return wrapIntegrity(&lifecycleStream{Stream: stream, ctx: ctx}), nil
}

type lifecycleStream struct {
	Stream
	ctx       context.Context
	mu        sync.Mutex
	response  ChatResponse
	finishing chan struct{}
	finishErr error
	closeOnce sync.Once
	closeErr  error
}

func (s *lifecycleStream) end(outcome string, err error) error {
	s.mu.Lock()
	if s.finishing != nil {
		// A concurrent end path must observe the same terminal record instead
		// of returning before the first caller finished persisting it.
		finished := s.finishing
		s.mu.Unlock()
		<-finished
		s.mu.Lock()
		finishErr := s.finishErr
		s.mu.Unlock()
		return finishErr
	}
	finished := make(chan struct{})
	s.finishing = finished
	response := s.response
	s.mu.Unlock()
	err = errors.Join(err, lifecycleFailure(s.ctx))
	if err != nil && outcome == "completed" {
		outcome = "failed"
	}
	finishErr := errors.Join(lifecycleFailure(s.ctx), endInvocation(s.ctx, &response, outcome, err))
	s.mu.Lock()
	s.finishErr = finishErr
	s.mu.Unlock()
	close(finished)
	return finishErr
}
func (s *lifecycleStream) Recv() (*Chunk, error) {
	chunk, err := s.Stream.Recv()
	s.mu.Lock()
	if chunk != nil {
		if chunk.Model != "" {
			s.response.Model = chunk.Model
		}
		if chunk.Usage != nil {
			s.response.Usage = *chunk.Usage
		}
		if chunk.FinishReason != "" {
			s.response.FinishReason = chunk.FinishReason
		}
	}
	s.mu.Unlock()
	if err != nil {
		outcome := callOutcome(err)
		if errors.Is(err, io.EOF) {
			outcome = "completed"
		}
		terminalErr := err
		if errors.Is(err, io.EOF) {
			terminalErr = nil
		}
		if finishErr := s.end(outcome, terminalErr); finishErr != nil {
			if errors.Is(err, io.EOF) {
				return chunk, finishErr
			}
			return chunk, errors.Join(err, finishErr)
		}
	}
	return chunk, err
}
func (s *lifecycleStream) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.Stream.Close()
		s.closeErr = errors.Join(s.closeErr, s.end("interrupted", s.closeErr))
	})
	return s.closeErr
}

func acquireAttempt(ctx context.Context, cfg *RetryConfig, rateLimitStarted time.Time) (AttemptPermit, error) {
	if err := lifecycleFailure(ctx); err != nil {
		return nil, err
	}
	permit, err := acquireQuotaAttempt(ctx, cfg, rateLimitStarted)
	if err != nil {
		return nil, err
	}
	inv := InvocationFromContext(ctx)
	if inv.InvocationID == "" {
		return permit, nil
	}
	id := AttemptIDFromContext(ctx)
	event := LifecycleEvent{ID: id + ":start", Kind: "attempt_started", Invocation: inv, AttemptID: id, Time: time.Now().UTC()}
	if err := recordLifecycle(ctx, event); err != nil {
		finishAttempt(permit, AttemptResult{Err: err, ErrorClass: ErrorClassPermanent})
		return nil, err
	}
	return &lifecyclePermit{inner: permit, ctx: ctx, id: id, inv: inv}, nil
}

type lifecyclePermit struct {
	inner AttemptPermit
	ctx   context.Context
	id    string
	inv   Invocation
	once  sync.Once
}

func (p *lifecyclePermit) Finish(result AttemptResult) {
	p.once.Do(func() {
		finishAttempt(p.inner, result)
		outcome := "failed"
		if result.Success {
			outcome = "completed"
		} else if result.Err == nil {
			outcome = "interrupted"
		} else {
			outcome = callOutcome(result.Err)
		}
		_ = recordLifecycle(p.ctx, LifecycleEvent{ID: p.id + ":end", Kind: "attempt_finished", Invocation: p.inv, AttemptID: p.id, Time: time.Now().UTC(), Outcome: outcome, ErrorClass: result.ErrorClass, Err: result.Err})
	})
}

// ModelObserverProvider lets a storage-bound factory preserve its mandatory
// observer through standard helpers and transparent model wrappers.
type ModelObserverProvider interface{ LifecycleObserver() LifecycleObserver }

func modelObserverContext(ctx context.Context, model ChatModel) context.Context {
	if provider, ok := model.(ModelObserverProvider); ok {
		if observer := provider.LifecycleObserver(); observer != nil {
			return WithLifecycleObserver(ctx, observer)
		}
	}
	return ctx
}
