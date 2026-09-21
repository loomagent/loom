package review

import (
	"context"
	"errors"
	"strings"
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

type failingWriter struct{ recordingWriter }

func (*failingWriter) WriteReasoning(context.Context, string, string) error {
	return errors.New("sink down")
}

func TestNewRequiresAReviewerAndFillsDefaults(t *testing.T) {
	if _, err := New(Config{}); err == nil || !strings.Contains(err.Error(), "Reviewer is required") {
		t.Fatalf("error = %v", err)
	}
	policy, err := New(Config{Reviewer: ReviewerFunc(func(context.Context, Request) (Assessment, error) {
		return Assessment{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if policy.cfg.EveryToolRounds != 1 || policy.cfg.ReasoningLabel != "reviewer" {
		t.Fatalf("defaults = %+v", policy.cfg)
	}
}

// The reviewer's summary is written for the user to read, and a sink that cannot take it
// is a failure rather than something to shrug off.
func TestAfterToolsReportsReviewerAndWriterFailures(t *testing.T) {
	reviewerErr := errors.New("reviewer unavailable")
	policy, err := New(Config{Reviewer: ReviewerFunc(func(context.Context, Request) (Assessment, error) {
		return Assessment{}, reviewerErr
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.AfterTools(context.Background(), &recordingWriter{}, react.State{}, nil); !errors.Is(err, reviewerErr) {
		t.Fatalf("reviewer error = %v", err)
	}

	policy, err = New(Config{Reviewer: ReviewerFunc(func(context.Context, Request) (Assessment, error) {
		return Assessment{Summary: "looks fine"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.AfterTools(context.Background(), &failingWriter{}, react.State{}, nil); err == nil ||
		!strings.Contains(err.Error(), "sink down") {
		t.Fatalf("writer error = %v", err)
	}
}

// A review that runs on a schedule skips the rounds between, and still counts them.
func TestAfterToolsSkipsRoundsBetween(t *testing.T) {
	reviews := 0
	policy, err := New(Config{
		Reviewer:        ReviewerFunc(func(context.Context, Request) (Assessment, error) { reviews++; return Assessment{}, nil }),
		EveryToolRounds: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 4; round++ {
		if _, err := policy.AfterTools(context.Background(), &recordingWriter{}, react.State{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if reviews != 2 {
		t.Fatalf("reviews = %d, want every second round", reviews)
	}
}

// A sufficient assessment stops the run when asked, and an insufficient one continues with
// the instruction the configuration supplies when the reviewer gave none.
func TestAfterToolsStopsOnSufficiencyAndContinuesWithTheConfiguredInstruction(t *testing.T) {
	policy, err := New(Config{
		Reviewer: ReviewerFunc(func(context.Context, Request) (Assessment, error) {
			return Assessment{Sufficient: true, Summary: "enough"}, nil
		}),
		StopWhenSufficient:  true,
		ContinueInstruction: "made by the configuration",
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := policy.AfterTools(context.Background(), &recordingWriter{}, react.State{Messages: []loom.Message{{Role: loom.RoleUser, Content: "q"}}}, nil)
	if err != nil || !decision.Stop || decision.FinalContent != "enough" {
		t.Fatalf("decision = %+v err = %v", decision, err)
	}

	policy, err = New(Config{
		Reviewer: ReviewerFunc(func(context.Context, Request) (Assessment, error) {
			return Assessment{Sufficient: false, Summary: "not yet"}, nil
		}),
		ContinueInstruction: "made by the configuration",
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err = policy.AfterTools(context.Background(), &recordingWriter{}, react.State{Messages: []loom.Message{{Role: loom.RoleUser, Content: "q"}}}, nil)
	if err != nil || decision.Stop || len(decision.Messages) != 2 {
		t.Fatalf("decision = %+v err = %v", decision, err)
	}
	if decision.Messages[1].Content != "made by the configuration" {
		t.Fatalf("instruction = %+v", decision.Messages)
	}

	// Without either instruction there is nothing to say, so the loop simply continues.
	policy, err = New(Config{Reviewer: ReviewerFunc(func(context.Context, Request) (Assessment, error) {
		return Assessment{Sufficient: false}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	decision, err = policy.AfterTools(context.Background(), &recordingWriter{}, react.State{}, nil)
	if err != nil || decision.Stop || decision.Messages != nil {
		t.Fatalf("decision = %+v err = %v", decision, err)
	}
}

// Requiring a review before finishing makes an early finish continue, and a satisfied
// review lets it through.
func TestBeforeFinish(t *testing.T) {
	assessments := []Assessment{{Sufficient: false}}
	policy, err := New(Config{
		Reviewer:                  ReviewerFunc(func(context.Context, Request) (Assessment, error) { return assessments[0], nil }),
		RequireReviewBeforeFinish: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// No review has run yet, so a finish is refused with the default instruction.
	decision, err := policy.BeforeFinish(context.Background(), react.State{}, &loom.ChatResponse{})
	if err != nil || !decision.Continue || decision.Instruction == "" {
		t.Fatalf("decision = %+v err = %v", decision, err)
	}
	if _, err := policy.AfterTools(context.Background(), &recordingWriter{}, react.State{}, nil); err != nil {
		t.Fatal(err)
	}
	decision, err = policy.BeforeFinish(context.Background(), react.State{}, &loom.ChatResponse{})
	if err != nil || !decision.Continue {
		t.Fatalf("an insufficient review must still refuse the finish: %+v err = %v", decision, err)
	}

	assessments[0] = Assessment{Sufficient: true}
	if _, err := policy.AfterTools(context.Background(), &recordingWriter{}, react.State{}, nil); err != nil {
		t.Fatal(err)
	}
	decision, err = policy.BeforeFinish(context.Background(), react.State{}, &loom.ChatResponse{})
	if err != nil || decision.Continue {
		t.Fatalf("a sufficient review must let the finish through: %+v err = %v", decision, err)
	}
}

// Without the requirement, a review that was never asked for must not block a finish.
func TestBeforeFinishWithoutTheRequirement(t *testing.T) {
	policy, err := New(Config{Reviewer: ReviewerFunc(func(context.Context, Request) (Assessment, error) {
		return Assessment{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := policy.BeforeFinish(context.Background(), react.State{}, &loom.ChatResponse{}); err != nil || decision.Continue {
		t.Fatalf("decision = %+v err = %v", decision, err)
	}
}
