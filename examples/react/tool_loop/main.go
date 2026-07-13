// Command tool_loop runs the smallest useful ReAct loop with local tools.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/examples/internal/exampleenv"
	"github.com/loomagent/loom/examples/internal/researchapp"
	"github.com/loomagent/loom/react"
	"github.com/loomagent/loom/tools/calculator"
	"github.com/loomagent/loom/tools/gettime"
)

func main() {
	question := flag.String("question", "What weekday is it in Beijing, and what is (17*23)+9?", "question for the agent")
	flag.Parse()
	model, err := exampleenv.OpenRouterModel()
	if err != nil {
		log.Fatal(err)
	}
	tools := loom.NewToolRegistry(gettime.New(), calculator.New())
	turn, err := loom.Run(context.Background(), func(ctx context.Context, writer loom.TurnWriter, _ []loom.Turn, input loom.UserMessage) error {
		result, err := react.Run(ctx, writer, react.Config{
			Model: model, Tools: tools,
			Messages: []loom.Message{
				{Role: loom.RoleSystem, Content: "Use tools for time and arithmetic. Answer concisely and do not guess tool results."},
				{Role: loom.RoleUser, Content: input.Text},
			},
			Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled}, MaxSteps: 4, MaxToolCalls: 4,
		})
		if err != nil {
			return err
		}
		return writer.FinalAnswer(ctx, result.FinalContent)
	}, loom.RunOptions{ConversationID: "example-tool-loop", Input: loom.UserMessage{Text: *question}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(researchapp.FinalAnswer(turn))
}
