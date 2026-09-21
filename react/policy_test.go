package react

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/loomagent/loom"
)

// lookupTool is a tool the loop can call, standing in for a real one.
func lookupTool() loom.Tool {
	return loom.NewArgsTool(loom.MustArgsContract("lookup"), "lookup", func(context.Context, loom.Args) (string, error) {
		return "ok", nil
	})
}

// runInTurn executes react.Run inside a real Turn, which is what a caller does and what
// gives the loop a Writer to write through.
func runInTurn(t *testing.T, cfg Config) (*Result, error) {
	t.Helper()
	var result *Result
	_, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var runErr error
		result, runErr = Run(ctx, w, cfg)
		if runErr != nil {
			return runErr
		}
		return w.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "react-policy", Input: loom.UserMessage{Text: "go"}})
	return result, err
}

func TestRunRequiresItsInputs(t *testing.T) {
	model := &scriptedModel{responses: []*loom.ChatResponse{{Content: "ok", FinishReason: loom.FinishReasonStop}}}
	if _, err := Run(context.Background(), nil, Config{Model: model, Tools: loom.NewToolRegistry()}); err == nil ||
		!strings.Contains(err.Error(), "Writer is required") {
		t.Fatalf("nil writer error = %v", err)
	}
	if _, err := Run(context.Background(), nil, Config{Tools: loom.NewToolRegistry()}); err == nil ||
		!strings.Contains(err.Error(), "Model is required") {
		t.Fatalf("nil model error = %v", err)
	}
	if _, err := Run(context.Background(), nil, Config{Model: model}); err == nil ||
		!strings.Contains(err.Error(), "Tools is required") {
		t.Fatalf("nil tools error = %v", err)
	}
}

// A step policy sees the turn so far and can rewrite what is about to be sent.
func TestStepPolicyRewritesTheStep(t *testing.T) {
	model := &scriptedModel{responses: []*loom.ChatResponse{{Content: "answer", FinishReason: loom.FinishReasonStop}}}
	seen := 0
	result, err := runInTurn(t, Config{
		Model: model,
		Tools: loom.NewToolRegistry(),
		StepPolicies: []StepPolicy{StepPolicyFunc(func(_ context.Context, state State, plan *StepPlan) error {
			seen++
			if state.Step != 0 || state.ToolCallsUsed != 0 || len(state.Messages) != len(plan.Messages) {
				t.Errorf("state = %+v", state)
			}
			plan.Messages = append(plan.Messages, loom.Message{Role: loom.RoleSystem, Content: "policy instruction"})
			plan.Tools = nil
			plan.ToolChoice = &loom.ToolChoice{Mode: loom.ToolChoiceNone}
			return nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 1 || result.FinalContent != "answer" {
		t.Fatalf("seen=%d result=%+v", seen, result)
	}
	request := model.requests[0]
	if len(request.Messages) != 1 || request.Messages[0].Content != "policy instruction" {
		t.Fatalf("policy messages not sent: %+v", request.Messages)
	}
	if len(request.Tools) != 0 || request.ToolChoice == nil || request.ToolChoice.Mode != loom.ToolChoiceNone {
		t.Fatalf("policy tools not sent: %v %+v", request.Tools, request.ToolChoice)
	}
}

func TestStepPolicyErrorStopsTheLoop(t *testing.T) {
	_, err := runInTurn(t, Config{
		Model: &scriptedModel{responses: []*loom.ChatResponse{{Content: "x", FinishReason: loom.FinishReasonStop}}},
		Tools: loom.NewToolRegistry(),
		StepPolicies: []StepPolicy{StepPolicyFunc(func(context.Context, State, *StepPlan) error {
			return errors.New("policy said no")
		})},
	})
	if err == nil || !strings.Contains(err.Error(), "step 1 policy: policy said no") {
		t.Fatalf("error = %v", err)
	}
}

// A finish policy may reject a finish once and continue with an instruction, and a failure
// in the policy itself stops the loop.
func TestFinishPolicyErrorStopsTheLoop(t *testing.T) {
	_, err := runInTurn(t, Config{
		Model: &scriptedModel{responses: []*loom.ChatResponse{{Content: "x", FinishReason: loom.FinishReasonStop}}},
		Tools: loom.NewToolRegistry(),
		FinishPolicies: []FinishPolicy{FinishPolicyFunc(func(context.Context, State, *loom.ChatResponse) (FinishDecision, error) {
			return FinishDecision{}, errors.New("reviewer unavailable")
		})},
	})
	if err == nil || !strings.Contains(err.Error(), "finish policy: reviewer unavailable") {
		t.Fatalf("error = %v", err)
	}
}

// An after-tools policy may replace what the next step sees and may stop the loop with an
// answer of its own.
func TestAfterToolsPolicyReplacesMessagesAndStops(t *testing.T) {
	response := &loom.ChatResponse{
		FinishReason: loom.FinishReasonToolCalls,
		ToolCalls:    []loom.ToolCall{{ID: "c1", Name: "lookup", Arguments: `{}`}},
	}
	model := &scriptedModel{responses: []*loom.ChatResponse{response}}
	registry := loom.NewToolRegistry(lookupTool())
	result, err := runInTurn(t, Config{
		Model: model,
		Tools: registry,
		AfterToolsPolicies: []AfterToolsPolicy{AfterToolsPolicyFunc(func(_ context.Context, _ loom.Writer, state State, results []loom.ToolExecResult) (AfterToolsDecision, error) {
			if len(results) != 1 || results[0].Call.ID != "c1" {
				t.Errorf("results = %+v", results)
			}
			return AfterToolsDecision{
				Messages:     append(state.Messages, loom.Message{Role: loom.RoleSystem, Content: "reviewed"}),
				Stop:         true,
				FinalContent: "stopped by policy",
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A policy stop is not a soft landing: nothing was trimmed away to reach it.
	if result.SoftLanded {
		t.Fatalf("result = %+v", result)
	}
	if result.FinalContent != "stopped by policy" || result.Steps != 1 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Messages) == 0 || result.Messages[len(result.Messages)-1].Content != "reviewed" {
		t.Fatalf("policy messages not kept: %+v", result.Messages)
	}
}

func TestAfterToolsPolicyErrorStopsTheLoop(t *testing.T) {
	response := &loom.ChatResponse{
		FinishReason: loom.FinishReasonToolCalls,
		ToolCalls:    []loom.ToolCall{{ID: "c1", Name: "lookup", Arguments: `{}`}},
	}
	_, err := runInTurn(t, Config{
		Model: &scriptedModel{responses: []*loom.ChatResponse{response}},
		Tools: loom.NewToolRegistry(lookupTool()),
		AfterToolsPolicies: []AfterToolsPolicy{AfterToolsPolicyFunc(func(context.Context, loom.Writer, State, []loom.ToolExecResult) (AfterToolsDecision, error) {
			return AfterToolsDecision{}, errors.New("review failed")
		})},
	})
	if err == nil || !strings.Contains(err.Error(), "after-tools policy: review failed") {
		t.Fatalf("error = %v", err)
	}
}

// The finish reason is the model's own report of why it stopped, and each one means
// something different to the caller upstream.
func TestValidateFinishReason(t *testing.T) {
	for _, reason := range []loom.FinishReason{"", loom.FinishReasonStop, loom.FinishReasonToolCalls} {
		if err := validateFinishReason(reason); err != nil {
			t.Errorf("validateFinishReason(%q) = %v, want success", reason, err)
		}
	}
	if err := validateFinishReason(loom.FinishReasonContentFilter); !errors.Is(err, loom.ErrContentFilter) {
		t.Errorf("content filter = %v", err)
	}
	if err := validateFinishReason(loom.FinishReasonLength); !errors.Is(err, loom.ErrOutputTruncated) {
		t.Errorf("length = %v", err)
	}
	if err := validateFinishReason(loom.FinishReasonError); err == nil || !strings.Contains(err.Error(), "error finish reason") {
		t.Errorf("error reason = %v", err)
	}
	// An unrecognised reason is reported as one rather than treated as a clean stop.
	if err := validateFinishReason(loom.FinishReason("other")); err == nil || !strings.Contains(err.Error(), "unknown finish reason") {
		t.Errorf("unknown reason = %v", err)
	}
}

// A total tool budget is a budget for the whole run, so the calls past it are refused
// rather than dropped, and the tools stop being offered.
func TestToolCallsBeyondTheTotalBudgetAreRefused(t *testing.T) {
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{
			FinishReason: loom.FinishReasonToolCalls,
			ToolCalls: []loom.ToolCall{
				{ID: "c1", Name: "lookup", Arguments: `{}`},
				{ID: "c2", Name: "lookup", Arguments: `{}`},
			},
		},
		{Content: "done", FinishReason: loom.FinishReasonStop},
	}}
	result, err := runInTurn(t, Config{
		Model:        model,
		Tools:        loom.NewToolRegistry(lookupTool()),
		MaxToolCalls: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalContent != "done" {
		t.Fatalf("result = %+v", result)
	}
	// The second call was refused, so the first step's model turn saw the rejection as a
	// failed tool result rather than silence.
	second := model.requests[1]
	found := false
	for _, message := range second.Messages {
		if message.Role == loom.RoleTool && strings.Contains(message.Content, "tool call limit 1 reached") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal was not reported to the model: %+v", second.Messages)
	}
	if len(second.Tools) != 0 {
		t.Fatalf("an exhausted budget still offered tools: %+v", second.Tools)
	}
}

// A writer that is down cannot be allowed to look like a refused tool call.
func TestRejectedToolCallReportsAWriterFailure(t *testing.T) {
	// Two calls and a budget of one, so the second is refused and the refusal is written.
	response := &loom.ChatResponse{
		FinishReason: loom.FinishReasonToolCalls,
		ToolCalls: []loom.ToolCall{
			{ID: "c1", Name: "lookup", Arguments: `{}`},
			{ID: "c2", Name: "lookup", Arguments: `{}`},
		},
	}
	for _, failing := range []failingWriter{{failToolCall: true}, {failToolResult: true}} {
		_, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
			_, runErr := Run(ctx, failing.wrap(w), Config{
				Model:        &scriptedModel{responses: []*loom.ChatResponse{response}},
				Tools:        loom.NewToolRegistry(lookupTool()),
				MaxToolCalls: 1,
			})
			return runErr
		}, loom.RunOptions{ConversationID: "sink-down"})
		if err == nil || !strings.Contains(err.Error(), "sink down") {
			t.Fatalf("writer failure = %v", err)
		}
	}
}

// failingWriter fails one write, which is how a Sink that is down behaves.
type failingWriter struct {
	failToolCall   bool
	failToolResult bool
}

func (f failingWriter) wrap(w loom.Writer) loom.Writer {
	return failingWrite{Writer: w, failToolCall: f.failToolCall, failToolResult: f.failToolResult}
}

type failingWrite struct {
	loom.Writer
	failToolCall   bool
	failToolResult bool
}

func (w failingWrite) WriteToolCall(ctx context.Context, label string, call loom.ToolCall) error {
	if w.failToolCall {
		return errors.New("sink down")
	}
	return w.Writer.WriteToolCall(ctx, label, call)
}

func (w failingWrite) WriteToolResult(ctx context.Context, label string, result loom.ToolResult) error {
	if w.failToolResult {
		return errors.New("sink down")
	}
	return w.Writer.WriteToolResult(ctx, label, result)
}
