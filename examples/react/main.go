// Command react runs the ReAct loop against a scripted model: it calls a tool once, then
// answers. The script stands in for a provider, so the example needs no API key and the same
// loop code works against a real model.
//
//	go run ./examples/react
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/react"
)

func main() {
	// One declaration per argument, written once: it is the tool's schema, the validator
	// the model's arguments are checked against, and what the handler reads.
	query := loom.String("query").Required().MinLen(1).Desc("Term to look up.")
	contract := loom.MustArgsContract("lookup", query)
	tool := loom.NewArgsTool(contract, "Look a term up in the local glossary.",
		func(_ context.Context, args loom.Args) (string, error) {
			return fmt.Sprintf(`{"term":%q,"definition":"a target that produces no file of its own"}`, query.Get(args)), nil
		})

	finalize := loom.NewArgsTool(loom.MustArgsContract("finalize_answer"),
		"End the tool phase on its own when no more tools are needed.",
		func(context.Context, loom.Args) (string, error) { return `{"ok":true}`, nil },
		loom.WithEndsToolPhase())
	model := &scriptedModel{responses: []*loom.ChatResponse{
		{
			ReasoningContent: "The question is about a build term, so look it up.",
			ToolCalls:        []loom.ToolCall{{ID: "call_1", Name: "lookup", Arguments: `{"query":"phony target"}`}},
			FinishReason:     loom.FinishReasonToolCalls,
		},
		{ToolCalls: []loom.ToolCall{{ID: "call_2", Name: "finalize_answer", Arguments: `{}`}}, FinishReason: loom.FinishReasonToolCalls},
		{ReasoningContent: "Use the glossary definition.", Content: "A phony target is a build rule that produces no file of its own.", FinishReason: loom.FinishReasonStop},
	}}

	sink := loom.NewMemorySink()
	turn, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		_, err := react.RunToFinalAnswer(ctx, w, react.Config{
			Model:     model,
			Tools:     loom.NewToolRegistry(tool, finalize),
			Purpose:   "glossary",
			Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled},
		})
		return err
	}, loom.RunOptions{
		ConversationID: "react-example",
		Input:          loom.UserMessage{Text: "What is a phony target?"},
		Sinks:          []loom.Sink{sink},
		StrictSink:     true,
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("turn %s (%s) in %d steps\n", turn.Status, turn.CloseReason.Code, model.steps)
	for _, item := range turn.Items {
		printItem(item, "  ")
	}
	for _, call := range sink.LLMCalls() {
		fmt.Printf("  model call: %s purpose=%s tokens=%d\n", call.Model, call.Purpose, call.Usage.TotalTokens)
	}
	fmt.Printf("  turn total: %d tokens over %d calls\n", turn.Usage.TotalTokens, len(sink.LLMCalls()))
}

// scriptedModel replays the responses a provider would stream, and records what it was asked
// so the example can show the loop's shape.
type scriptedModel struct {
	responses []*loom.ChatResponse
	steps     int
}

func (m *scriptedModel) Name() string                         { return "example/scripted" }
func (m *scriptedModel) Capabilities() loom.ModelCapabilities { return loom.ModelCapabilities{} }

func (m *scriptedModel) Chat(context.Context, loom.ChatRequest) (*loom.ChatResponse, error) {
	return nil, errors.New("this example streams only")
}

func (m *scriptedModel) Stream(_ context.Context, _ loom.ChatRequest) (loom.Stream, error) {
	if len(m.responses) == 0 {
		return nil, errors.New("the script has run out of responses")
	}
	response := m.responses[0]
	m.responses = m.responses[1:]
	m.steps++

	// Emit reasoning and content in separate increments, then finish with usage.
	// This fake provider needs no credentials and makes no network requests.
	usage := loom.Usage{PromptTokens: 24, CompletionTokens: 12, TotalTokens: 36}
	chunk := &loom.Chunk{
		FinishReason: response.FinishReason,
		Model:        m.Name(),
		Usage:        &usage,
	}
	for index, call := range response.ToolCalls {
		chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, loom.ToolCallDelta{
			Index: index, ID: call.ID, Name: call.Name, Arguments: call.Arguments,
		})
	}
	var chunks []*loom.Chunk
	for _, r := range response.ReasoningContent {
		chunks = append(chunks, &loom.Chunk{ReasoningContentDelta: string(r)})
	}
	for _, r := range response.Content {
		chunks = append(chunks, &loom.Chunk{ContentDelta: string(r)})
	}
	chunks = append(chunks, chunk)
	return &sliceStream{chunks: chunks}, nil
}

type sliceStream struct{ chunks []*loom.Chunk }

func (s *sliceStream) Recv() (*loom.Chunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	chunk := s.chunks[0]
	s.chunks = s.chunks[1:]
	return chunk, nil
}

func (s *sliceStream) Close() error { return nil }

func printItem(item loom.Item, indent string) {
	label := item.Text
	if item.Label != "" {
		label = item.Label
	}
	switch item.Kind {
	case loom.ItemKindToolResult:
		label = item.Output
	case loom.ItemKindToolCall:
		label = item.ToolName + "(" + item.Arguments + ")"
	}
	if label == "" {
		label = "—"
	}
	fmt.Printf("%s%-13s %s\n", indent, item.Kind, label)
	for _, child := range item.Children {
		printItem(child, indent+"  ")
	}
}
