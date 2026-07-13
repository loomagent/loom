// Command deep_research_chat runs an interactive multi-turn research session.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/loomagent/loom/examples/internal/exampleenv"
	"github.com/loomagent/loom/examples/internal/researchapp"
	"github.com/loomagent/loom/providers/serper"
	"github.com/loomagent/loom/providers/unifuncs"
)

func main() {
	workspace := flag.String("workspace", filepath.Join(os.TempDir(), "loom-research-chat"), "persistent research workspace")
	flag.Parse()
	model, err := exampleenv.OpenRouterModel()
	if err != nil {
		log.Fatal(err)
	}
	app, err := researchapp.New(researchapp.Config{
		ConversationID: "example-research-chat", WorkspaceDir: *workspace, Model: model,
		Searcher: serper.New(required("SERPER_API_KEY")), Reader: unifuncs.New(required("UNIFUNCS_API_KEY")),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer app.Close()

	fmt.Println("Multi-turn deep research. Ask a question; follow up using the same SRC-N references. Type /quit to exit.")
	fmt.Printf("Workspace: %s\n", *workspace)
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\nresearch> ")
		if !scanner.Scan() {
			break
		}
		question := strings.TrimSpace(scanner.Text())
		if question == "" {
			continue
		}
		if question == "/quit" {
			break
		}
		turn, err := app.RunTurn(context.Background(), question)
		if err != nil {
			log.Printf("turn failed: %v", err)
			continue
		}
		fmt.Printf("\n%s\n", researchapp.FinalAnswer(turn))
		count, err := app.SourceCount(context.Background())
		if err == nil {
			fmt.Printf("\n[stable sources: %d]\n", count)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
}

func required(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("set %s", name)
	}
	return value
}
