package loom

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// blockingModel waits for its ctx, which is how a per-attempt deadline is exercised.
type blockingModel struct{}

func (m *blockingModel) Name() string                                        { return "test/blocking" }
func (m *blockingModel) Capabilities() ModelCapabilities                     { return ModelCapabilities{} }
func (m *blockingModel) Stream(context.Context, ChatRequest) (Stream, error) { return nil, io.EOF }

func (m *blockingModel) Chat(ctx context.Context, _ ChatRequest) (*ChatResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// One attempt is the default, and its error stays exactly what that call produced.
func TestStructuredAttemptsStayUnwrappedByDefault(t *testing.T) {
	t.Parallel()
	contract, _, _ := reviewContract()
	model := &fakeStructuredModel{
		responses: []string{`{"overall_done":"no","notes":"bad type"}`},
	}
	_, _, err := ChatStructuredArgs(t.Context(), "test.one", model, ChatRequest{}, contract)
	if _, ok := errors.AsType[*StructuredOutputError](err); !ok {
		t.Fatalf("err = %v, want a StructuredOutputError", err)
	}
	if _, ok := errors.AsType[*StructuredAttemptsError](err); ok {
		t.Fatalf("a single attempt must not be wrapped: %v", err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(model.requests))
	}
}

// Asking for more attempts without saying what the next request is fails before calling anything:
// the framework does not choose what the second request says.
func TestStructuredAttemptsRequireANextRequest(t *testing.T) {
	t.Parallel()
	contract, _, _ := reviewContract()
	model := &fakeStructuredModel{responses: []string{`{"overall_done":true,"notes":"ok"}`}}
	_, _, err := ChatStructuredArgs(t.Context(), "test.need-callback", model, ChatRequest{}, contract,
		WithStructuredAttempts(2))
	if err == nil || !strings.Contains(err.Error(), "WithStructuredNextRequest is required") {
		t.Fatalf("err = %v", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("the call must not happen: %d requests", len(model.requests))
	}
}

// The callback sees the attempt that failed and decides what the next one sends.
func TestStructuredNextRequestDrivesTheNextAttempt(t *testing.T) {
	t.Parallel()
	contract, done, notes := reviewContract()
	model := &fakeStructuredModel{responses: []string{
		`{"overall_done":"no","notes":"bad type"}`,
		`{"overall_done":true,"notes":"ok"}`,
	}}
	var attempts int
	args, _, err := ChatStructuredArgs(t.Context(), "test.next", model, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "review"}},
	}, contract,
		WithStructuredAttempts(2),
		WithStructuredNextRequest(func(_ context.Context, attempt StructuredAttempt) (*ChatRequest, error) {
			attempts++
			if attempt.Number != 1 {
				t.Errorf("attempt number = %d, want 1", attempt.Number)
			}
			if attempt.Contract != contract {
				t.Error("the callback must see the contract")
			}
			outputErr, ok := errors.AsType[*StructuredOutputError](attempt.Err)
			if !ok || !strings.Contains(outputErr.Content, "bad type") {
				t.Errorf("attempt error = %v", attempt.Err)
			}
			if attempt.Response == nil || !strings.Contains(attempt.Response.Content, "bad type") {
				t.Errorf("attempt response = %+v", attempt.Response)
			}
			if len(attempt.Request.Messages) != 1 {
				t.Fatalf("attempt request = %+v", attempt.Request)
			}
			// The request is a copy: mutating it must not reach the request that was sent.
			attempt.Request.Messages[0].Content = "mutated"
			next := attempt.Request
			next.Messages = append(next.Messages,
				Message{Role: RoleAssistant, Content: outputErr.Content},
				Message{Role: RoleUser, Content: "return one JSON value matching the contract"})
			return &next, nil
		}))
	if err != nil {
		t.Fatalf("ChatStructuredArgs: %v", err)
	}
	if attempts != 1 || !done.Get(args) || notes.Get(args) != "ok" {
		t.Fatalf("attempts = %d, args = %+v", attempts, args)
	}
	if len(model.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(model.requests))
	}
	if got := model.requests[0].Messages[0].Content; got != "review" {
		t.Fatalf("the callback mutated the request that was sent: %q", got)
	}
	second := model.requests[1].Messages
	if len(second) != 3 || second[1].Role != RoleAssistant || second[2].Role != RoleUser {
		t.Fatalf("the second request must carry what the callback added: %+v", second)
	}
}

// Stopping from the callback reports the last failure and how many attempts were spent.
func TestStructuredNextRequestStopsTheCall(t *testing.T) {
	t.Parallel()
	contract, _, _ := reviewContract()
	model := &fakeStructuredModel{responses: []string{
		`{"overall_done":"no","notes":"bad type"}`,
		`{"overall_done":true,"notes":"ok"}`,
	}}
	_, _, err := ChatStructuredArgs(t.Context(), "test.stop", model, ChatRequest{}, contract,
		WithStructuredAttempts(3),
		WithStructuredNextRequest(func(context.Context, StructuredAttempt) (*ChatRequest, error) {
			return nil, nil
		}))
	var attemptsErr *StructuredAttemptsError
	if !errors.As(err, &attemptsErr) || attemptsErr.Attempts != 3 {
		t.Fatalf("err = %v, want a StructuredAttemptsError for 3 attempts", err)
	}
	if _, ok := errors.AsType[*StructuredOutputError](err); !ok {
		t.Fatalf("the last failure must stay reachable: %v", err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("requests = %d, want 1: the callback stopped the call", len(model.requests))
	}
}

// A callback that fails is its own error, with the model failure kept apart.
func TestStructuredNextRequestFailureIsReportedSeparately(t *testing.T) {
	t.Parallel()
	contract, _, _ := reviewContract()
	model := &fakeStructuredModel{responses: []string{`{"overall_done":"no","notes":"bad type"}`}}
	callbackErr := errors.New("the caller's own bug")
	_, _, err := ChatStructuredArgs(t.Context(), "test.callback-error", model, ChatRequest{}, contract,
		WithStructuredAttempts(2),
		WithStructuredNextRequest(func(context.Context, StructuredAttempt) (*ChatRequest, error) {
			return nil, callbackErr
		}))
	callbackFailure, ok := errors.AsType[*StructuredNextRequestError](err)
	if !ok {
		t.Fatalf("err = %v, want a StructuredNextRequestError", err)
	}
	if !errors.Is(err, callbackErr) {
		t.Fatalf("the callback error must stay reachable: %v", err)
	}
	if _, ok := errors.AsType[*StructuredOutputError](callbackFailure.ModelErr); !ok {
		t.Fatalf("the model failure must be kept apart: %v", callbackFailure.ModelErr)
	}
}

// An attempt's own deadline is named as such, so a callback can tell it from a failed request.
func TestStructuredAttemptTimeoutIsItsOwnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		contract, _, _ := reviewContract()
		var seen error
		done := make(chan error, 1)
		go func() {
			_, _, err := ChatStructuredArgs(context.Background(), "test.timeout", &blockingModel{}, ChatRequest{}, contract,
				WithStructuredAttempts(2),
				WithStructuredAttemptTimeout(time.Minute),
				WithStructuredNextRequest(func(_ context.Context, attempt StructuredAttempt) (*ChatRequest, error) {
					seen = attempt.Err
					return nil, nil
				}))
			done <- err
		}()
		synctest.Wait()
		synctest.Sleep(2 * time.Minute)
		err := <-done
		if !errors.Is(err, ErrAttemptTimeout) {
			t.Fatalf("err = %v, want ErrAttemptTimeout", err)
		}
		if !errors.Is(seen, ErrAttemptTimeout) {
			t.Fatalf("the callback saw %v, want the attempt timeout", seen)
		}
	})
}

// A caller's cancelled ctx ends the call without asking the callback to continue.
func TestStructuredAttemptsStopOnParentCancel(t *testing.T) {
	t.Parallel()
	contract, _, _ := reviewContract()
	model := &fakeStructuredModel{responses: []string{`{"overall_done":true,"notes":"ok"}`}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, _, err := ChatStructuredArgs(ctx, "test.parent-cancel", model, ChatRequest{}, contract,
		WithStructuredAttempts(2),
		WithStructuredNextRequest(func(context.Context, StructuredAttempt) (*ChatRequest, error) {
			called = true
			return nil, nil
		}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("the callback must not be asked to continue a cancelled run")
	}
}
