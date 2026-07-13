// Command deep_research runs one evidence-backed web research turn.
package main

import (
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
	question := flag.String("question", "Compare the strongest public evidence for and against small modular reactors becoming cost-competitive before 2035.", "research question")
	workspace := flag.String("workspace", filepath.Join(os.TempDir(), "loom-deep-research"), "persistent research workspace")
	flag.Parse()
	model, err := exampleenv.OpenRouterModel()
	if err != nil {
		log.Fatal(err)
	}
	app, err := researchapp.New(researchapp.Config{
		ConversationID: "example-deep-research", WorkspaceDir: *workspace, Model: model,
		Searcher: serper.New(requireEnv("SERPER_API_KEY")), Reader: unifuncs.New(requireEnv("UNIFUNCS_API_KEY")),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer app.Close()
	turn, err := app.RunTurn(context.Background(), *question)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(researchapp.FinalAnswer(turn))
	fmt.Printf("\nWorkspace: %s\n", *workspace)
}

func requireEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("set %s", name)
	}
	return value
}
