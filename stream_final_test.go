package loom

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"slices"
	"testing"
	"testing/synctest"
)

type finalStreamModel struct {
	frames     []*Chunk
	err        error
	streamErr  error
	beforeRecv func(int)
	caps       ModelCapabilities
}

func (*finalStreamModel) Name() string                      { return "test/final" }
func (m *finalStreamModel) Capabilities() ModelCapabilities { return m.caps }
func (*finalStreamModel) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	return nil, errors.New("streaming required")
}
func (m *finalStreamModel) Stream(context.Context, ChatRequest) (Stream, error) {
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	return &finalFrameStream{model: m}, nil
}

type finalFrameStream struct {
	model *finalStreamModel
	index int
}

func (s *finalFrameStream) Recv() (*Chunk, error) {
	if s.model.beforeRecv != nil {
		s.model.beforeRecv(s.index)
	}
	if s.index == len(s.model.frames) {
		if s.model.err != nil {
			return nil, s.model.err
		}
		return nil, io.EOF
	}
	frame := s.model.frames[s.index]
	s.index++
	return frame, nil
}
func (*finalFrameStream) Close() error { return nil }

func TestUserFinalResponseStreamsOptionalReasoningAndContent(t *testing.T) {
	for _, tc := range []struct {
		name          string
		frames        []*Chunk
		wantReasoning string
		wantKinds     []ItemKind
		wantChunks    []string
	}{
		{"without_reasoning", []*Chunk{{ContentDelta: "hello"}, {ContentDelta: " world"}}, "", []ItemKind{ItemKindFinalAnswer}, []string{"hello", " world"}},
		{"with_reasoning", []*Chunk{{ReasoningContentDelta: "think"}, {ReasoningContentDelta: " carefully"}, {ContentDelta: "hello"}, {ContentDelta: " world"}}, "think carefully", []ItemKind{ItemKindReasoning, ItemKindFinalAnswer}, []string{"think", " carefully", "hello", " world"}},
		{"mixed_frame", []*Chunk{{ReasoningContentDelta: "think", ContentDelta: "hello"}, {ContentDelta: " world"}}, "think", []ItemKind{ItemKindReasoning, ItemKindFinalAnswer}, []string{"think", "hello", " world"}},
		{"interleaved", []*Chunk{{ContentDelta: "hello"}, {ReasoningContentDelta: "think"}, {ContentDelta: " world"}}, "think", []ItemKind{ItemKindFinalAnswer, ItemKindReasoning}, []string{"hello", "think", " world"}},
	} {
		t.Run("user_"+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given provider frames with optional reasoning, including a mixed frame.
				sink := NewMemorySink()
				frames := append(slices.Clone(tc.frames), &Chunk{FinishReason: FinishReasonStop, Model: "test/final", ReasoningDetails: jsontext.Value(`[{"type":"reasoning","data":"opaque"}]`), Usage: &Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}})
				model := &finalStreamModel{frames: frames}
				model.beforeRecv = func(next int) {
					// Then all preceding nonempty deltas are visible before another frame is read.
					var want []string
					for _, frame := range frames[:next] {
						if frame.ReasoningContentDelta != "" {
							want = append(want, frame.ReasoningContentDelta)
						}
						if frame.ContentDelta != "" {
							want = append(want, frame.ContentDelta)
						}
					}
					var got []string
					for _, event := range sink.DeltaEvents() {
						got = append(got, event.Chunk)
					}
					if !slices.Equal(got, want) {
						t.Errorf("deltas delayed: got=%v want=%v", got, want)
					}
				}
				var response *ChatResponse
				// When the user requests the final response through the real Turn writer.
				turn, err := Run(t.Context(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
					var err error
					response, err = StreamLLMToFinalAnswer(ctx, w, "final", model, ChatRequest{Reasoning: Reasoning{Mode: ReasoningModeEnabled}})
					return err
				}, RunOptions{ConversationID: tc.name, Sinks: []Sink{sink}, StrictSink: true})
				if err != nil || turn.Status != TurnStatusCompleted {
					t.Fatalf("turn=%+v err=%v", turn, err)
				}
				if response.Content != "hello world" || response.ReasoningContent != tc.wantReasoning || string(response.ReasoningDetails) != `[{"type":"reasoning","data":"opaque"}]` {
					t.Fatalf("response=%+v", response)
				}
				var kinds []ItemKind
				for _, item := range turn.Items {
					kinds = append(kinds, item.Kind)
				}
				if !slices.Equal(kinds, tc.wantKinds) {
					t.Fatalf("items=%v", kinds)
				}
				if turn.Usage.TotalTokens != 5 || len(sink.LLMCalls()) != 1 {
					t.Fatalf("usage=%+v calls=%v", turn.Usage, sink.LLMCalls())
				}
			})
		})
	}
}

func TestUserFinalStreamFailuresCannotCommit(t *testing.T) {
	broken := errors.New("provider disconnected")
	for _, tc := range []struct {
		name    string
		frames  []*Chunk
		recvErr error
		want    error
	}{
		{"empty", []*Chunk{{FinishReason: FinishReasonStop}}, nil, ErrEmptyFinalAnswer},
		{"reasoning_only", []*Chunk{{ReasoningContentDelta: "think", FinishReason: FinishReasonStop}}, nil, ErrEmptyFinalAnswer},
		{"whitespace", []*Chunk{{ContentDelta: " ", FinishReason: FinishReasonStop}}, nil, ErrEmptyFinalAnswer},
		{"truncated", []*Chunk{{ContentDelta: "partial", FinishReason: FinishReasonLength}}, nil, ErrOutputTruncated},
		{"filtered", []*Chunk{{ContentDelta: "partial", FinishReason: FinishReasonContentFilter}}, nil, ErrContentFilter},
		{"tool_finish", []*Chunk{{ContentDelta: "partial", FinishReason: FinishReasonToolCalls}}, nil, ErrToolCallInFinalAnswer},
		{"tool_delta", []*Chunk{{ContentDelta: "partial"}, {ToolCallDeltas: []ToolCallDelta{{Name: "unexpected"}}}}, nil, ErrToolCallInFinalAnswer},
		{"missing_stop", []*Chunk{{ContentDelta: "partial"}}, nil, nil},
		{"unknown_stop", []*Chunk{{ContentDelta: "partial", FinishReason: "unknown"}}, nil, nil},
		{"broken_reasoning", []*Chunk{{ReasoningContentDelta: "partial"}}, broken, broken},
		{"broken_answer", []*Chunk{{ContentDelta: "partial", Usage: &Usage{CompletionTokens: 2}}}, broken, broken},
		{"nil_frames", []*Chunk{nil}, broken, broken},
	} {
		t.Run("user_"+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given an interrupted or invalid provider response, when it is consumed,
				// then no successful final answer is committed, while partial text is retained.
				model := &finalStreamModel{frames: tc.frames, err: tc.recvErr}
				turn, err := Run(t.Context(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
					_, err := StreamLLMToFinalAnswer(ctx, w, "final", model, ChatRequest{Reasoning: Reasoning{Mode: ReasoningModeDisabled}})
					return err
				}, RunOptions{ConversationID: tc.name})
				if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || turn.Status != TurnStatusFailed {
					t.Fatalf("turn=%+v err=%v", turn, err)
				}
				for _, item := range turn.Items {
					if item.Kind == ItemKindFinalAnswer && item.Status == ItemStatusCompleted {
						t.Fatal("invalid answer committed")
					}
				}
				if tc.name == "broken_answer" && turn.Usage.CompletionTokens != 2 {
					t.Fatal("partial usage lost")
				}
			})
		})
	}
}

type finalFailSink struct {
	*MemorySink
	at  string
	err error
}

func (s *finalFailSink) ItemStarted(ctx context.Context, e ItemStartedEvent) error {
	if s.at == "start" {
		return s.err
	}
	return s.MemorySink.ItemStarted(ctx, e)
}
func (s *finalFailSink) ItemDelta(ctx context.Context, e ItemDeltaEvent) error {
	if s.at == "delta" {
		return s.err
	}
	return s.MemorySink.ItemDelta(ctx, e)
}
func (s *finalFailSink) ItemFinished(ctx context.Context, e ItemFinishedEvent) error {
	if s.at == "finish" {
		return s.err
	}
	return s.MemorySink.ItemFinished(ctx, e)
}
func (s *finalFailSink) LLMCalled(ctx context.Context, e LLMCalledEvent) error {
	if s.at == "usage" {
		return s.err
	}
	return s.MemorySink.LLMCalled(ctx, e)
}

func TestUserPersistenceFailureDoesNotReportSuccessfulAnswer(t *testing.T) {
	for _, at := range []string{"start", "delta", "finish", "usage"} {
		t.Run("user_"+at, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given a mandatory sink that cannot persist an event, when streaming,
				// then the turn fails with the sink error rather than claiming delivery.
				failed := errors.New("storage unavailable")
				sink := &finalFailSink{MemorySink: NewMemorySink(), at: at, err: failed}
				model := &finalStreamModel{frames: []*Chunk{{ContentDelta: "answer", FinishReason: FinishReasonStop, Usage: &Usage{CompletionTokens: 1}}}}
				turn, err := Run(t.Context(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
					_, err := StreamLLMToFinalAnswer(ctx, w, "final", model, ChatRequest{Reasoning: Reasoning{Mode: ReasoningModeDisabled}})
					return err
				}, RunOptions{ConversationID: at, Sinks: []Sink{sink}, StrictSink: true})
				if !errors.Is(err, failed) || turn.Status != TurnStatusFailed || !errors.Is(turn.CloseReason.Cause, failed) {
					t.Fatalf("turn=%+v err=%v", turn, err)
				}
			})
		})
	}
}

func TestUserCancellationKeepsPartialAnswerUncommitted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given a response canceled after its first content delta, when the stream
		// ends, then cancellation remains the terminal state and the partial item is canceled.
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		model := &finalStreamModel{frames: []*Chunk{{ContentDelta: "partial"}}, beforeRecv: func(index int) {
			if index == 1 {
				cancel()
			}
		}}
		turn, err := Run(ctx, func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
			_, err := StreamLLMToFinalAnswer(ctx, w, "final", model, ChatRequest{Reasoning: Reasoning{Mode: ReasoningModeDisabled}})
			return err
		}, RunOptions{ConversationID: "canceled"})
		if err != nil || turn.Status != TurnStatusCancelled || turn.Items[0].Status != ItemStatusCancelled || turn.Items[0].Text != "partial" {
			t.Fatalf("turn=%+v err=%v", turn, err)
		}
	})
}

func TestUserFinalRequestValidation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given a final request, unsupported configuration is rejected before any model stream.
		model := &finalStreamModel{streamErr: errors.New("stream unavailable")}
		_, err := StreamLLMToFinalAnswer(t.Context(), nil, "final", model, ChatRequest{})
		if err == nil {
			t.Fatal("nil writer accepted")
		}
		_, _ = Run(t.Context(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
			for _, req := range []ChatRequest{
				{Tools: []*ToolInfo{{Name: "lookup"}}},
				{ToolChoice: &ToolChoice{Mode: ToolChoiceRequired}},
				{},
			} {
				if _, err := StreamLLMToFinalAnswer(ctx, w, "final", model, req); err == nil {
					t.Error("invalid request accepted")
				}
			}
			if _, err := StreamLLMToFinalAnswer(ctx, w, "final", nil, ChatRequest{}); err == nil {
				t.Error("nil model accepted")
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			req := ChatRequest{Reasoning: Reasoning{Mode: ReasoningModeDisabled}}
			if _, err := StreamLLMToFinalAnswer(canceled, w, "final", model, req); !errors.Is(err, context.Canceled) {
				t.Errorf("err=%v", err)
			}
			if _, err := StreamLLMToFinalAnswer(ctx, w, "final", model, req); !errors.Is(err, model.streamErr) {
				t.Errorf("err=%v", err)
			}
			return nil
		}, RunOptions{ConversationID: "validation"})
	})
}

func TestUserCannotStartAnotherFinalResponseAfterCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given an already committed answer, when another generation is requested,
		// then the writer refuses it before contacting the model.
		model := &finalStreamModel{streamErr: errors.New("model must not be contacted")}
		turn, err := Run(t.Context(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
			req := ChatRequest{Reasoning: Reasoning{Mode: ReasoningModeDisabled}}
			return w.StreamFinalAnswer(ctx, func(fs FinalAnswerStream) error {
				if _, err := StreamLLMToFinalAnswer(ctx, w, "final", model, req); !errors.Is(err, ErrFinalAnswerInProgress) {
					t.Errorf("err=%v", err)
				}
				return fs.AppendText(ctx, "committed")
			})
		}, RunOptions{ConversationID: "busy-final"})
		if err != nil || turn.Status != TurnStatusCompleted {
			t.Fatalf("turn=%+v err=%v", turn, err)
		}
		_, err = Run(t.Context(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
			if err := w.FinalAnswer(ctx, "committed"); err != nil {
				return err
			}
			_, err := StreamLLMToFinalAnswer(ctx, w, "final", model, ChatRequest{Reasoning: Reasoning{Mode: ReasoningModeDisabled}})
			if !errors.Is(err, ErrTurnClosed) {
				t.Errorf("err=%v", err)
			}
			return nil
		}, RunOptions{ConversationID: "sealed-final"})
		if err != nil {
			t.Fatal(err)
		}
	})
}
