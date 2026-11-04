package loom

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"testing"
)

type infoTestModel struct {
	ChatModel
	info   ModelInfo
	before func()
}

func (m *infoTestModel) ModelInfo() ModelInfo { return m.info }
func (m *infoTestModel) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if m.before != nil {
		m.before()
	}
	return m.ChatModel.Chat(ctx, req)
}
func (m *infoTestModel) Stream(ctx context.Context, req ChatRequest) (Stream, error) {
	if m.before != nil {
		m.before()
	}
	return m.ChatModel.Stream(ctx, req)
}

func TestDescribeModelKeepsUnknownIdentity(t *testing.T) {
	for _, model := range []ChatModel{nil, &fakeCallModel{name: "deepseek/same-alias"}} {
		if got := DescribeModel(model); got != (ModelInfo{}) {
			t.Fatalf("inferred binding: %+v", got)
		}
	}
	model := &infoTestModel{info: ModelInfo{Ref: ModelRef{InstanceID: "instance", SnapshotID: "snapshot"}, Provider: "test", RequestModel: "alias"}}
	info := DescribeModel(model)
	info.Ref.SnapshotID = "modified"
	if DescribeModel(model).Ref.SnapshotID != "snapshot" {
		t.Fatal("returned metadata aliases model")
	}
}

func TestCallModelCapturesEachFailoverTarget(t *testing.T) {
	for _, paid := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmetered failure", true: "paid failure"}[paid], func(t *testing.T) {
			sink := NewMemorySink()
			failure := errors.New("primary failed")
			primaryInfo := ModelInfo{Ref: ModelRef{InstanceID: "account-A:alias", SnapshotID: "snapshot-A"}, Provider: "test", RequestModel: "alias"}
			fallbackInfo := ModelInfo{Ref: ModelRef{InstanceID: "account-B:alias", SnapshotID: "snapshot-B"}, Provider: "test", RequestModel: "alias"}
			var primaryResponse *ChatResponse
			if paid {
				primaryResponse = &ChatResponse{Model: "provider-revision-A", Usage: Usage{PromptTokens: 7}}
			}
			primary := &infoTestModel{ChatModel: &fakeCallModel{name: "test/alias", responses: []*ChatResponse{primaryResponse}, errs: []error{failure}}, info: primaryInfo}
			fallback := &infoTestModel{ChatModel: &fakeCallModel{name: "test/alias", responses: []*ChatResponse{{Content: "ok", Usage: Usage{PromptTokens: 9}}}}, info: fallbackInfo}
			// Metadata is captured before invocation, even with a mutable custom model.
			primary.before = func() { primary.info.Ref.SnapshotID = "reloaded" }
			_, err := Run(t.Context(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
				_, err := CallModel(ctx, "auxiliary.review", primary, ChatRequest{Messages: []Message{{Role: RoleUser, Content: "question"}}}, WithModelFailover(FailoverConfig{
					GetFailoverModel: func(_ context.Context, attempt FailoverAttempt) (ChatModel, error) {
						if attempt.ModelInfo != primaryInfo || !errors.Is(attempt.Error, failure) {
							t.Fatalf("failed attempt lost identity: %+v", attempt)
						}
						return fallback, nil
					},
				}))
				if err != nil {
					return err
				}
				return w.FinalAnswer(ctx, "done")
			}, RunOptions{ConversationID: "identity", Sinks: []Sink{sink}, Input: UserMessage{Text: "question"}})
			if err != nil {
				t.Fatal(err)
			}
			calls := sink.LLMCalls()
			wantCount := 1
			if paid {
				wantCount = 2
			}
			if len(calls) != wantCount {
				t.Fatalf("usage events=%d want %d", len(calls), wantCount)
			}
			last := calls[len(calls)-1]
			if last.ModelInfo != fallbackInfo || last.ProviderReportedModel != "" || last.Purpose != "auxiliary.review" {
				t.Fatalf("fallback attributed to wrong target: %+v", last)
			}
			if paid && (calls[0].ModelInfo != primaryInfo || calls[0].ProviderReportedModel != "provider-revision-A" || calls[0].Model != "provider-revision-A") {
				t.Fatalf("primary metadata changed: %+v", calls[0])
			}
			wire, err := jsonv2.Marshal(last)
			if err != nil {
				t.Fatal(err)
			}
			var roundtrip LLMCalledEvent
			if err := jsonv2.Unmarshal(wire, &roundtrip); err != nil {
				t.Fatal(err)
			}
			if roundtrip.ModelInfo != fallbackInfo {
				t.Fatal("event lost binding on JSON roundtrip")
			}
			t.Logf("paid_primary=%t fallback_event=%s", paid, wire)
		})
	}
}

func TestStreamingEventsCaptureConfiguredTarget(t *testing.T) {
	for _, final := range []bool{false, true} {
		for _, reported := range []string{"", "remote-revision"} {
			t.Run(map[bool]string{false: "research/", true: "final/"}[final]+reported, func(t *testing.T) {
				sink := NewMemorySink()
				info := ModelInfo{Ref: ModelRef{InstanceID: "account:alias", SnapshotID: "snapshot"}, Provider: "test", RequestModel: "alias"}
				model := &infoTestModel{ChatModel: &finalStreamModel{frames: []*Chunk{{ContentDelta: "answer"}, {Model: reported, Usage: &Usage{PromptTokens: 11}, FinishReason: FinishReasonStop}}}, info: info}
				model.before = func() { model.info.Ref.SnapshotID = "updated" }
				_, err := Run(t.Context(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
					req := ChatRequest{Messages: []Message{{Role: RoleUser, Content: "question"}}, Reasoning: Reasoning{Mode: ReasoningModeDisabled}}
					if final {
						_, err := StreamLLMToFinalAnswer(ctx, w, "react.final", model, req)
						return err
					}
					resp, err := StreamLLMToStep(ctx, w, "react.main", model, req)
					if err != nil {
						return err
					}
					return w.FinalAnswer(ctx, resp.Content)
				}, RunOptions{ConversationID: "stream-identity", Sinks: []Sink{sink}, Input: UserMessage{Text: "question"}})
				if err != nil {
					t.Fatal(err)
				}
				calls := sink.LLMCalls()
				if len(calls) != 1 || calls[0].ModelInfo != info || calls[0].ProviderReportedModel != reported {
					t.Fatalf("stream metadata wrong: %+v", calls)
				}
			})
		}
	}
}
