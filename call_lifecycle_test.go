package loom

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestLifecycleSelectedRequestEvidenceNeverSerializesContent(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"chat", "research stream", "final stream", "direct observe"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request := ChatRequest{Messages: []Message{{Role: RoleUser, Content: "private-request-marker"}}, Reasoning: Reasoning{Mode: ReasoningModeDisabled}, MaxTokens: new(123)}
			starts := 0
			ctx := WithLifecycleObserver(t.Context(), LifecycleObserverFunc(func(_ context.Context, event LifecycleEvent) error {
				if event.Kind == "call_started" {
					starts++
					if event.Request == nil || !reflect.DeepEqual(*event.Request, request) {
						t.Fatalf("selected request evidence=%+v", event.Request)
					}
				}
				data, err := json.Marshal(event)
				if err != nil {
					return err
				}
				if strings.Contains(string(data), "private-request-marker") || strings.Contains(string(data), "Messages") {
					t.Fatal("transient input leaked into serialized lifecycle")
				}
				return nil
			}))
			var err error
			switch name {
			case "chat":
				_, err = CallModel(ctx, "test", &fakeCallModel{responses: []*ChatResponse{{Content: "answer"}}}, request)
			case "direct observe":
				_, err = ObserveChat(ctx, ModelInfo{}, func(context.Context) (*ChatResponse, error) { return &ChatResponse{}, nil }, WithObservedRequest(request))
			default:
				_, err = Run(ctx, func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
					model := &mockChatModel{chunks: []*Chunk{{ContentDelta: "answer", FinishReason: FinishReasonStop}}}
					if name == "final stream" {
						_, err := StreamLLMToFinalAnswer(ctx, w, "final", model, request)
						return err
					}
					response, err := StreamLLMToStep(ctx, w, "research", model, request)
					if err != nil {
						return err
					}
					return w.FinalAnswer(ctx, response.Content)
				}, RunOptions{ConversationID: name})
			}
			if err != nil || starts != 1 {
				t.Fatalf("starts=%d err=%v", starts, err)
			}
		})
	}
}

type lifecycleLog struct {
	mu       sync.Mutex
	events   []LifecycleEvent
	failKind string
	failure  error
}

func (l *lifecycleLog) Record(_ context.Context, e LifecycleEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.Kind == l.failKind {
		return l.failure
	}
	l.events = append(l.events, e)
	return nil
}
func TestLifecycleAuditFailuresStopCallsAndRetries(t *testing.T) {
	for _, kind := range []string{"call_started", "attempt_started", "attempt_finished", "call_finished"} {
		t.Run(kind, func(t *testing.T) {
			log := &lifecycleLog{failKind: kind, failure: errors.New("storage unavailable")}
			ctx := WithLifecycleObserver(t.Context(), log)
			requests := 0
			_, err := ObserveChat(ctx, ModelInfo{}, func(ctx context.Context) (*ChatResponse, error) {
				return ChatWithRetry(ctx, mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}, &RetryConfig{MaxRetries: 2, InitialBackoff: time.Nanosecond, MaxBackoff: time.Nanosecond}, func(ctx context.Context) (*ChatResponse, error) {
					requests++
					if AttemptIDFromContext(ctx) == "" {
						t.Fatal("missing attempt identity")
					}
					if kind == "attempt_finished" {
						return nil, errors.New("provider down")
					}
					return &ChatResponse{Content: "ok"}, nil
				})
			})
			var persistence *ObservationError
			if !errors.As(err, &persistence) || !errors.Is(err, log.failure) {
				t.Fatalf("mandatory write failure lost: %v", err)
			}
			want := 1
			if kind == "call_started" || kind == "attempt_started" {
				want = 0
			}
			if requests != want {
				t.Fatalf("storage failure allowed requests=%d want %d", requests, want)
			}
		})
	}
}
func TestLifecycleRetriesAndFailoverKeepDistinctIdentities(t *testing.T) {
	log := &lifecycleLog{}
	ctx := WithLifecycleObserver(WithCallScope(t.Context(), "research"), log)
	for _, ref := range []string{"primary", "fallback"} {
		count := 0
		_, err := ObserveChat(ctx, ModelInfo{Ref: ModelRef{InstanceID: ref, SnapshotID: "snapshot-" + ref}}, func(ctx context.Context) (*ChatResponse, error) {
			return ChatWithRetry(ctx, mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}, &RetryConfig{MaxRetries: 1, InitialBackoff: time.Nanosecond, MaxBackoff: time.Nanosecond}, func(context.Context) (*ChatResponse, error) {
				count++
				if count == 1 {
					return nil, errors.New("retry")
				}
				return &ChatResponse{Content: "ok", Usage: Usage{PromptTokens: 1}}, nil
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	calls, invocations, attempts := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range log.events {
		calls[e.Invocation.CallID] = true
		invocations[e.Invocation.InvocationID] = true
		if e.AttemptID != "" {
			attempts[e.AttemptID] = true
		}
		if e.Invocation.Purpose != "research" {
			t.Fatal("purpose lost")
		}
	}
	if len(calls) != 1 || len(invocations) != 2 || len(attempts) != 4 || len(log.events) != 12 {
		t.Fatalf("identity split incorrect: calls=%d invocations=%d attempts=%d events=%d", len(calls), len(invocations), len(attempts), len(log.events))
	}
}
func TestLifecycleStreamsDistinguishEOFAndEarlyClose(t *testing.T) {
	for _, early := range []bool{false, true} {
		for _, failEnd := range []bool{false, true} {
			t.Run(map[bool]string{false: "eof", true: "close"}[early]+map[bool]string{false: "/ok", true: "/write-failed"}[failEnd], func(t *testing.T) {
				log := &lifecycleLog{}
				if failEnd {
					log.failKind = "call_finished"
					log.failure = errors.New("disk down")
				}
				ctx := WithLifecycleObserver(t.Context(), log)
				stream, err := ObserveStream(ctx, ModelInfo{}, func(context.Context) (Stream, error) {
					return &mockStream{chunks: []*Chunk{{ContentDelta: "ok", Model: "remote", Usage: &Usage{PromptTokens: 7}}}}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := stream.Recv(); err != nil {
					t.Fatal(err)
				}
				if early {
					err = stream.Close()
				} else {
					_, err = stream.Recv()
				}
				if failEnd {
					var failure *ObservationError
					if !errors.As(err, &failure) || errors.Is(err, io.EOF) {
						t.Fatalf("audit failure became EOF: %v", err)
					}
				} else if !early && !errors.Is(err, io.EOF) {
					t.Fatalf("normal EOF changed: %v", err)
				} else if early && err != nil {
					t.Fatal(err)
				}
				_ = stream.Close()
				_ = stream.Close()
				if !failEnd {
					if len(log.events) != 2 {
						t.Fatalf("terminal event count=%d", len(log.events))
					}
					last := log.events[1]
					want := "completed"
					if early {
						want = "interrupted"
					}
					if last.Outcome != want || last.ProviderReportedModel != "remote" || last.Usage == nil || last.Usage.PromptTokens != 7 {
						t.Fatalf("stream evidence=%+v", last)
					}
				}
			})
		}
	}
}
func TestLifecyclePersistenceFailureDoesNotFailOver(t *testing.T) {
	log := &lifecycleLog{failKind: "call_started", failure: errors.New("storage down")}
	model := &fakeCallModel{name: "fake", responses: []*ChatResponse{{Content: "ok"}}}
	failovers := 0
	_, err := CallModel(WithLifecycleObserver(t.Context(), log), "test", model, ChatRequest{Messages: []Message{{Role: RoleUser, Content: "question"}}}, WithModelFailover(FailoverConfig{GetFailoverModel: func(context.Context, FailoverAttempt) (ChatModel, error) { failovers++; return model, nil }}))
	var failure *ObservationError
	if !errors.As(err, &failure) || failovers != 0 || len(model.requests) != 0 {
		t.Fatalf("audit outage failed over or sent request: err=%v fallback=%d requests=%d", err, failovers, len(model.requests))
	}
}

func TestLifecyclePersistenceFailureSkipsProviderBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := &lifecycleLog{failKind: "attempt_finished", failure: errors.New("audit down")}
		started := time.Now()
		_, err := ObserveChat(WithLifecycleObserver(t.Context(), log), ModelInfo{}, func(ctx context.Context) (*ChatResponse, error) {
			return ChatWithRetry(ctx, mockClassifier{classify: func(error) ErrorClass { return ErrorClassTransient }}, &RetryConfig{MaxRetries: 1, InitialBackoff: time.Hour, MaxBackoff: time.Hour}, func(context.Context) (*ChatResponse, error) { return nil, errors.New("provider down") })
		})
		if class, known := ErrorClassOf(err); !known || class != ErrorClassPermanent {
			t.Fatalf("audit failure classified as retriable: %v", err)
		}
		if time.Since(started) != 0 {
			t.Fatal("audit failure waited through provider backoff")
		}
	})
}

func TestLifecycleFailureIsNotStructuredRepairInput(t *testing.T) {
	log := &lifecycleLog{failKind: "call_finished", failure: errors.New("storage down")}
	model := &fakeStructuredModel{responses: []string{`{"overall_done":true,"notes":"ok"}`}}
	contract, _, _ := reviewContract()
	callbacks := 0
	_, _, err := ChatStructuredArgs(WithLifecycleObserver(t.Context(), log), "structured", model, ChatRequest{Messages: []Message{{Role: RoleUser, Content: "question"}}}, contract, WithStructuredAttempts(3), WithStructuredNextRequest(func(_ context.Context, a StructuredAttempt) (*ChatRequest, error) {
		callbacks++
		return new(a.Request), nil
	}))
	if _, ok := errors.AsType[*ObservationError](err); !ok || callbacks != 0 || len(model.requests) != 1 {
		t.Fatalf("audit failure triggered structured repair: err=%v callbacks=%d requests=%d", err, callbacks, len(model.requests))
	}
}
