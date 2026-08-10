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

func TestCallModel_UsageAccumulatesAtBoundTurnAndStep(t *testing.T) {
	sink := NewMemorySink()
	rootModel := &fakeCallModel{name: "provider/root", responses: []*ChatResponse{{
		Content: "root", Model: "actual/root", Usage: Usage{PromptTokens: 11, CompletionTokens: 3, CachedTokens: 4, TotalTokens: 14},
	}}}
	stepModel := &fakeCallModel{name: "provider/step", responses: []*ChatResponse{{
		Content: "step", Usage: Usage{PromptTokens: 17, CompletionTokens: 5, ReasoningTokens: 2, TotalTokens: 22},
	}}}
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		if _, err := CallModel(ctx, "sync.root", rootModel, ChatRequest{}); err != nil {
			return err
		}
		if err := w.Step(ctx, "structured", func(stepCtx context.Context, _ Step) error {
			_, err := CallModel(stepCtx, "sync.step", stepModel, ChatRequest{})
			return err
		}); err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "done")
	}, RunOptions{ConversationID: "conv_sync_usage", Sinks: []Sink{sink}, Input: UserMessage{Text: "x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := turn.Usage; got.PromptTokens != 28 || got.CompletionTokens != 8 || got.CachedTokens != 4 || got.ReasoningTokens != 2 || got.TotalTokens != 36 {
		t.Fatalf("turn usage=%+v", got)
	}
	if len(turn.Items) < 1 || turn.Items[0].Kind != ItemKindStep || turn.Items[0].Usage.PromptTokens != 17 || turn.Items[0].Usage.CompletionTokens != 5 {
		t.Fatalf("step usage not attributed: %+v", turn.Items)
	}
	calls := sink.LLMCalls()
	if len(calls) != 2 {
		t.Fatalf("llm calls=%d, want 2: %+v", len(calls), calls)
	}
	if calls[0].Model != "actual/root" || calls[0].StepPath != "" || calls[1].Model != "provider/step" || calls[1].StepPath == "" {
		t.Fatalf("unexpected usage attribution: %+v", calls)
	}
}

func TestCallModel_FailoverCountsEveryPaidResponse(t *testing.T) {
	sink := NewMemorySink()
	primary := &fakeCallModel{name: "primary", responses: []*ChatResponse{{
		Content: "blocked", FinishReason: FinishReasonContentFilter, Usage: Usage{PromptTokens: 7, CompletionTokens: 1, TotalTokens: 8},
	}}}
	fallback := &fakeCallModel{name: "fallback", responses: []*ChatResponse{{
		Content: "ok", FinishReason: FinishReasonStop, Usage: Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}}
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		_, err := CallModel(ctx, "sync.failover", primary, ChatRequest{}, WithModelFailover(FailoverConfig{
			ShouldFailover:   ShouldFailoverOnErrorOrFinishReason(FinishReasonContentFilter),
			GetFailoverModel: func(context.Context, FailoverAttempt) (ChatModel, error) { return fallback, nil },
		}))
		if err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "done")
	}, RunOptions{ConversationID: "conv_sync_failover", Sinks: []Sink{sink}, Input: UserMessage{Text: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Usage.PromptTokens != 16 || turn.Usage.CompletionTokens != 3 || turn.Usage.TotalTokens != 19 || len(sink.LLMCalls()) != 2 {
		t.Fatalf("paid failover usage lost: turn=%+v calls=%+v", turn.Usage, sink.LLMCalls())
	}
}
