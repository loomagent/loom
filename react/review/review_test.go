package review

import (
	"context"
	"testing"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/react"
)

type recordingWriter struct{ reasoning string }

func (*recordingWriter) Path() string { return "test" }
func (w *recordingWriter) WriteReasoning(_ context.Context, _, text string) error {
	w.reasoning = text
	return nil
}
func (*recordingWriter) WriteToolCall(context.Context, string, loom.ToolCall) error     { return nil }
func (*recordingWriter) WriteToolResult(context.Context, string, loom.ToolResult) error { return nil }
func (*recordingWriter) Step(context.Context, string, func(context.Context, loom.Step) error) error {
	return nil
}
func (*recordingWriter) StreamReasoning(context.Context, string, func(loom.ReasoningStream) error) error {
	return nil
}

func TestPolicyRequiresMoreWork(t *testing.T) {
	policy, err := New(Config{
		Reviewer: ReviewerFunc(func(context.Context, Request) (Assessment, error) {
			return Assessment{Summary: "more evidence needed", Instruction: "search another source"}, nil
		}),
		RequireReviewBeforeFinish: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &recordingWriter{}
	decision, err := policy.AfterTools(context.Background(), w, react.State{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.reasoning != "more evidence needed" || len(decision.Messages) != 1 {
		t.Fatalf("reasoning=%q decision=%+v", w.reasoning, decision)
	}
	finish, err := policy.BeforeFinish(context.Background(), react.State{}, &loom.ChatResponse{})
	if err != nil || !finish.Continue {
		t.Fatalf("finish=%+v err=%v", finish, err)
	}
}
