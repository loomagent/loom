// Command quickstart writes one turn and prints what a Sink received from it. It needs no
// API key: the handler is the agent, and the model never enters the picture.
//
//	go run ./examples/quickstart
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/loomagent/loom"
)

func main() {
	sink := loom.NewMemorySink()

	turn, err := loom.Run(context.Background(), func(
		ctx context.Context,
		w loom.TurnWriter,
		_ []loom.Turn,
		input loom.UserMessage,
	) error {
		if err := w.WriteReasoning(ctx, "Plan", "Answer directly."); err != nil {
			return err
		}
		// Steps nest, and a step may write anything the root may write except the
		// final answer.
		if err := w.Step(ctx, "Greeting", func(ctx context.Context, step loom.Step) error {
			return step.WriteReasoning(ctx, "Tone", "Warm, one line.")
		}); err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "Hello, "+input.Text+".")
	}, loom.RunOptions{
		ConversationID: "quickstart",
		Input:          loom.UserMessage{Text: "Loom"},
		Sinks:          []loom.Sink{sink},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("turn %s (%s)\n", turn.Status, turn.CloseReason.Code)
	for _, item := range turn.Items {
		printItem(item, "  ")
	}
	fmt.Printf("\nthe sink saw %d started, %d finished, %d delta events\n",
		len(sink.StartedEvents()), len(sink.FinishedEvents()), len(sink.DeltaEvents()))
	fmt.Println("note: Run does not persist the user message, so the caller stores it first")
}

// printItem walks the nested item tree the way a UI would.
func printItem(item loom.Item, indent string) {
	label := item.Text
	if item.Label != "" {
		label = item.Label + " — " + label
	}
	fmt.Printf("%s%-13s %s\n", indent, item.Kind, label)
	for _, child := range item.Children {
		printItem(child, indent+"  ")
	}
}
