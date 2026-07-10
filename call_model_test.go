package loom

import (
	"context"
	"errors"
	"io"
	"testing"
)

type fakeCallModel struct {
	name         string
	responses    []*ChatResponse
	errs         []error
	requests     []ChatRequest
	capabilities ModelCapabilities
}

func (m *fakeCallModel) Name() string {
	return m.name
}

func (m *fakeCallModel) Capabilities() ModelCapabilities {
	return m.capabilities
}

func (m *fakeCallModel) Chat(_ context.Context, req ChatRequest) (*ChatResponse, error) {
	m.requests = append(m.requests, req)
	var resp *ChatResponse
	var err error
	if len(m.responses) > 0 {
		resp = m.responses[0]
		m.responses = m.responses[1:]
	}
	if len(m.errs) > 0 {
		err = m.errs[0]
		m.errs = m.errs[1:]
	}
	return resp, err
}

func (m *fakeCallModel) Stream(context.Context, ChatRequest) (Stream, error) {
	return nil, io.EOF
}

func TestCallModel_FailoverOnError(t *testing.T) {
	primaryErr := errors.New("primary failed")
	primary := &fakeCallModel{name: "primary/model", errs: []error{primaryErr}}
	fallback := &fakeCallModel{name: "fallback/model", responses: []*ChatResponse{{Content: "ok", Model: "fallback-model"}}}

	resp, err := CallModel(
		context.Background(),
		"test.failover",
		primary,
		ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}},
		WithModelFailover(FailoverConfig{
			GetFailoverModel: func(_ context.Context, attempt FailoverAttempt) (ChatModel, error) {
				if attempt.Attempt != 1 {
					t.Fatalf("attempt = %d, want 1", attempt.Attempt)
				}
				if attempt.Model != primary {
					t.Fatalf("attempt model mismatch")
				}
				if !errors.Is(attempt.Error, primaryErr) {
					t.Fatalf("attempt error = %v", attempt.Error)
				}
				return fallback, nil
			},
		}),
	)
	if err != nil {
		t.Fatalf("CallModel() error = %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("resp = %#v", resp)
	}
	if len(primary.requests) != 1 || len(fallback.requests) != 1 {
		t.Fatalf("requests primary=%d fallback=%d", len(primary.requests), len(fallback.requests))
	}
}

func TestCallModel_FailoverOnFinishReason(t *testing.T) {
	primary := &fakeCallModel{
		name:      "primary/model",
		responses: []*ChatResponse{{Content: "blocked", FinishReason: FinishReasonContentFilter}},
	}
	fallback := &fakeCallModel{name: "fallback/model", responses: []*ChatResponse{{Content: "ok", FinishReason: FinishReasonStop}}}

	resp, err := CallModel(
		context.Background(),
		"test.failover",
		primary,
		ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}},
		WithModelFailover(FailoverConfig{
			ShouldFailover: ShouldFailoverOnErrorOrFinishReason(FinishReasonContentFilter),
			GetFailoverModel: func(context.Context, FailoverAttempt) (ChatModel, error) {
				return fallback, nil
			},
		}),
	)
	if err != nil {
		t.Fatalf("CallModel() error = %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("resp = %#v", resp)
	}
}

func TestCallModel_NoFailoverReturnsOriginalResponse(t *testing.T) {
	model := &fakeCallModel{
		name:      "primary/model",
		responses: []*ChatResponse{{Content: "blocked", FinishReason: FinishReasonContentFilter}},
	}
	resp, err := CallModel(context.Background(), "test", model, ChatRequest{})
	if err != nil {
		t.Fatalf("CallModel() error = %v", err)
	}
	if resp == nil || resp.Content != "blocked" {
		t.Fatalf("resp = %#v", resp)
	}
}
