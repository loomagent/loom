package loom

import (
	"context"
	"io"
	"testing"
)

// mockStream stands in for the Stream interface, returning a preset chunk sequence.
type mockStream struct {
	chunks []*Chunk
	idx    int
}

func (s *mockStream) Recv() (*Chunk, error) {
	if s.idx >= len(s.chunks) {
		return nil, io.EOF
	}
	c := s.chunks[s.idx]
	s.idx++
	return c, nil
}

func (s *mockStream) Close() error { return nil }

// mockChatModel implements ChatModel; Stream returns a preset mockStream.
type mockChatModel struct {
	chunks []*Chunk
}

func (m *mockChatModel) Name() string { return "mock" }

func (m *mockChatModel) Capabilities() ModelCapabilities { return ModelCapabilities{} }

func (m *mockChatModel) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	return nil, nil
}

func (m *mockChatModel) Stream(context.Context, ChatRequest) (Stream, error) {
	return &mockStream{chunks: m.chunks}, nil
}

func TestStreamLLMToStep_ReasoningAndContent(t *testing.T) {
	model := &mockChatModel{chunks: []*Chunk{
		{ReasoningContentDelta: "thinking"},
		{ReasoningContentDelta: " more"},
		{ContentDelta: "Hello"},
		{ContentDelta: " world"},
		{FinishReason: FinishReasonStop, Usage: &Usage{PromptTokens: 10, CompletionTokens: 5}},
	}}

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		resp, err := StreamLLMToStep(ctx, w, "test", model, ChatRequest{})
		if err != nil {
			return err
		}
		if resp.Content != "Hello world" {
			t.Errorf("content: %q", resp.Content)
		}
		if resp.FinishReason != FinishReasonStop {
			t.Errorf("finish: %s", resp.FinishReason)
		}
		if resp.Usage.PromptTokens != 10 {
			t.Errorf("usage: %+v", resp.Usage)
		}
		return w.FinalAnswer(ctx, resp.Content)
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	// items: reasoning / final_answer
	if len(turn.Items) != 2 {
		t.Fatalf("items: %d", len(turn.Items))
	}
	if turn.Items[0].Kind != ItemKindReasoning {
		t.Fatalf("[0].Kind: %s", turn.Items[0].Kind)
	}
	if turn.Items[0].Text != "thinking more" {
		t.Errorf("reasoning text: %q", turn.Items[0].Text)
	}
}

func TestStreamLLMToStep_ToolCallOnly(t *testing.T) {
	model := &mockChatModel{chunks: []*Chunk{
		{ToolCallDeltas: []ToolCallDelta{{Index: 0, ID: "c1", Name: "search", Arguments: `{"q":`}}},
		{ToolCallDeltas: []ToolCallDelta{{Index: 0, Arguments: `"ai"}`}}},
		{FinishReason: FinishReasonToolCalls},
	}}

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		resp, err := StreamLLMToStep(ctx, w, "test", model, ChatRequest{})
		if err != nil {
			return err
		}
		if len(resp.ToolCalls) != 1 {
			t.Fatalf("tool_calls: %d", len(resp.ToolCalls))
		}
		if resp.ToolCalls[0].Arguments != `{"q":"ai"}` {
			t.Errorf("args: %q", resp.ToolCalls[0].Arguments)
		}
		if resp.ToolCalls[0].ID != "c1" || resp.ToolCalls[0].Name != "search" {
			t.Errorf("call: %+v", resp.ToolCalls[0])
		}
		return w.FinalAnswer(ctx, "done")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	// No reasoning, because the model returned none, so no reasoning item.
	// No tool_call item: StreamLLMToStep leaves it to a later ExecuteToolCalls.
	// items: final_answer
	if len(turn.Items) != 1 {
		t.Fatalf("items: %d, want 1 (only final_answer); items=%+v", len(turn.Items), turn.Items)
	}
	if turn.Items[0].Kind != ItemKindFinalAnswer {
		t.Errorf("[0].Kind: %s", turn.Items[0].Kind)
	}
}

func TestStreamLLMToStep_ReasoningPlusToolCall(t *testing.T) {
	model := &mockChatModel{chunks: []*Chunk{
		{ReasoningContentDelta: "I need to search"},
		{ToolCallDeltas: []ToolCallDelta{{Index: 0, ID: "c1", Name: "search", Arguments: `{}`}}},
		{FinishReason: FinishReasonToolCalls},
	}}

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, h []Turn, in UserMessage) error {
		resp, err := StreamLLMToStep(ctx, w, "test", model, ChatRequest{})
		if err != nil {
			return err
		}
		if len(resp.ToolCalls) != 1 {
			t.Errorf("tool_calls: %d", len(resp.ToolCalls))
		}
		return w.FinalAnswer(ctx, "done")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	// No tool_call item: StreamLLMToStep leaves it to a later ExecuteToolCalls.
	// items: reasoning / final_answer
	if len(turn.Items) != 2 {
		t.Fatalf("items: %d, want 2 (reasoning + final_answer); items=%+v", len(turn.Items), turn.Items)
	}
	if turn.Items[0].Kind != ItemKindReasoning || turn.Items[0].Text != "I need to search" {
		t.Errorf("reasoning: %+v", turn.Items[0])
	}
	if turn.Items[1].Kind != ItemKindFinalAnswer {
		t.Errorf("final_answer: %+v", turn.Items[1])
	}
}

// TestStreamLLMToStep_UsageAccumulation checks token accumulation: turn.Usage is the
// sum over every LLM call, a nested step's own Item.Usage is the sum over its subtree,
// and sibling steps accumulate independently without contaminating each other.
func TestStreamLLMToStep_UsageAccumulation(t *testing.T) {
	makeModel := func(prompt, completion uint64) *mockChatModel {
		return &mockChatModel{chunks: []*Chunk{
			{ContentDelta: "ok"},
			{FinishReason: FinishReasonStop, Usage: &Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}},
		}}
	}

	turn, _ := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		// LLM call #1, hanging off the turn root
		if _, err := StreamLLMToStep(ctx, w, "root", makeModel(10, 5), ChatRequest{}); err != nil {
			return err
		}
		// step A: two LLM calls, with step C nested inside
		if err := w.Step(ctx, "A", func(ctx context.Context, stepA Step) error {
			if _, err := StreamLLMToStep(ctx, stepA, "step_a", makeModel(20, 10), ChatRequest{}); err != nil {
				return err
			}
			return stepA.Step(ctx, "C", func(ctx context.Context, stepC Step) error {
				_, err := StreamLLMToStep(ctx, stepC, "step_c", makeModel(30, 15), ChatRequest{})
				return err
			})
		}); err != nil {
			return err
		}
		// step B: one LLM call, a sibling of A
		if err := w.Step(ctx, "B", func(ctx context.Context, stepB Step) error {
			_, err := StreamLLMToStep(ctx, stepB, "step_b", makeModel(40, 20), ChatRequest{})
			return err
		}); err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "done")
	}, RunOptions{ConversationID: "conv_test", Input: UserMessage{Text: "x"}})

	if turn == nil {
		t.Fatalf("Run returned nil turn")
	}
	// What each level should accumulate:
	//   turn  = 10 + 20 + 30 + 40 = 100 prompt / 5+10+15+20 = 50 completion
	//   stepA = 20 + 30 = 50 prompt, 10 + 15 = 25 completion, including child step C
	//   stepC = 30 prompt / 15 completion
	//   stepB = 40 prompt / 20 completion
	if got := turn.Usage; got.PromptTokens != 100 || got.CompletionTokens != 50 {
		t.Errorf("turn.Usage = %+v, want prompt=100 completion=50", got)
	}

	// top-level items: no reasoning, since none of the calls reasoned, plus step A, step B, final_answer
	// LLM #1 hangs off the turn root, but it produced only content, no reasoning, and
	// the answer came through a final_answer, so that call leaves no item of its own:
	// the content stays in the ChatResponse for product code. The top-level items are
	// therefore step A, step B, and final_answer.
	if len(turn.Items) != 3 {
		t.Fatalf("items: %d, want 3 (stepA + stepB + final_answer); items=%+v", len(turn.Items), turn.Items)
	}
	stepA := &turn.Items[0]
	stepB := &turn.Items[1]
	if stepA.Kind != ItemKindStep || stepA.Label != "A" {
		t.Fatalf("[0] = %+v, want step A", stepA)
	}
	if stepB.Kind != ItemKindStep || stepB.Label != "B" {
		t.Fatalf("[1] = %+v, want step B", stepB)
	}
	if got := stepA.Usage; got.PromptTokens != 50 || got.CompletionTokens != 25 {
		t.Errorf("stepA.Usage = %+v, want prompt=50 completion=25", got)
	}
	if got := stepB.Usage; got.PromptTokens != 40 || got.CompletionTokens != 20 {
		t.Errorf("stepB.Usage = %+v, want prompt=40 completion=20", got)
	}
	// stepA.Children holds the nested stepC, which should show prompt=30, completion=15
	if len(stepA.Children) != 1 {
		t.Fatalf("stepA.Children: %d, want 1 (nested step C)", len(stepA.Children))
	}
	stepC := &stepA.Children[0]
	if stepC.Kind != ItemKindStep || stepC.Label != "C" {
		t.Fatalf("stepC = %+v, want step C", stepC)
	}
	if got := stepC.Usage; got.PromptTokens != 30 || got.CompletionTokens != 15 {
		t.Errorf("stepC.Usage = %+v, want prompt=30 completion=15", got)
	}
}
