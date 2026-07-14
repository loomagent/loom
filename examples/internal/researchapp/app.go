// Package researchapp contains shared plumbing for the runnable ReAct research
// examples. It is internal so applications copy the composition they need
// instead of depending on an example as a framework.
package researchapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/loomfs"
	"github.com/loomagent/loom/react"
	"github.com/loomagent/loom/sourceregistry"
	"github.com/loomagent/loom/tools/calculator"
	"github.com/loomagent/loom/tools/gettime"
	"github.com/loomagent/loom/tools/web"
	"github.com/loomagent/loom/tools/workspacebash"
	"github.com/loomagent/loom/tools/workspacebash/gobash"
)

// Config configures a research conversation.
type Config struct {
	ConversationID string
	WorkspaceDir   string
	Model          loom.ChatModel
	Searcher       web.WebSearcher
	Reader         web.WebReader
	MaxSteps       uint64
	MaxToolCalls   uint64
}

// App owns state shared across turns: history, source numbering, and workspace.
type App struct {
	conversationID string
	model          loom.ChatModel
	workspace      *loomfs.Workspace
	sources        *sourceregistry.Registry
	tools          *loom.ToolRegistry
	runner         *gobash.Runner
	history        []loom.Turn
	maxSteps       uint64
	maxToolCalls   uint64
}

// New constructs a multi-turn research application.
func New(cfg Config) (*App, error) {
	if strings.TrimSpace(cfg.ConversationID) == "" {
		return nil, errors.New("research example: ConversationID is required")
	}
	if cfg.Model == nil || cfg.Searcher == nil || cfg.Reader == nil {
		return nil, errors.New("research example: Model, Searcher, and Reader are required")
	}
	workspace, err := loomfs.OpenWorkspace(cfg.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	sources, err := sourceregistry.New(cfg.ConversationID, sourceregistry.NewMemoryStore())
	if err != nil {
		return nil, err
	}
	runner, err := gobash.New(gobash.Options{WorkspaceDir: workspace.Root()})
	if err != nil {
		return nil, err
	}
	searchTool := newSearchTool(cfg.Searcher)
	readerTool := newReaderTool(cfg.Reader)
	bashTool := workspacebash.NewTool(workspacebash.ToolOptions{
		Runner:           runner,
		Description:      "Inspect the persistent research workspace with read-only shell commands. Useful files include queries.jsonl, sources.jsonl, prior_turns.jsonl, context/turn_contexts.jsonl, and raw/SRC-N.md.",
		ErrorOnTruncated: true,
		MaxStdout:        200 * 1024,
		MaxStderr:        16 * 1024,
	})
	maxSteps := cfg.MaxSteps
	if maxSteps == 0 {
		maxSteps = 10
	}
	maxToolCalls := cfg.MaxToolCalls
	if maxToolCalls == 0 {
		maxToolCalls = 16
	}
	return &App{
		conversationID: cfg.ConversationID,
		model:          cfg.Model,
		workspace:      workspace,
		sources:        sources,
		tools:          loom.NewToolRegistry(searchTool, readerTool, bashTool, calculator.New(), gettime.New()),
		runner:         runner,
		maxSteps:       maxSteps,
		maxToolCalls:   maxToolCalls,
	}, nil
}

// Close releases the read-only workspace runner.
func (a *App) Close() error {
	if a == nil || a.runner == nil {
		return nil
	}
	return a.runner.Close()
}

// RunTurn executes one user turn while retaining history, source references,
// and workspace artifacts for subsequent turns.
func (a *App) RunTurn(ctx context.Context, userText string) (*loom.Turn, error) {
	userText = strings.TrimSpace(userText)
	if userText == "" {
		return nil, errors.New("research example: user text is required")
	}
	turnNumber := uint64(len(a.history) + 1)
	session, err := a.workspace.BeginTurn(loomfs.TurnMeta{
		ConversationID: a.conversationID,
		TurnIndex:      turnNumber,
		ChatModeID:     "deep_research",
		Executor:       "react",
		UserText:       userText,
	})
	if err != nil {
		return nil, err
	}
	snapshot, err := session.Snapshot(ctx, loomfs.SnapshotOptions{})
	if err != nil {
		return nil, err
	}
	contextBlock := loomfs.RenderContextBlock(snapshot, loomfs.RenderOptions{})
	messages := []loom.Message{{Role: loom.RoleSystem, Content: researchSystemPrompt}}
	if contextBlock != "" {
		messages = append(messages, loom.Message{Role: loom.RoleSystem, Content: contextBlock})
	}
	messages = append(messages, loom.Message{Role: loom.RoleUser, Content: userText})

	turnCtx := loomfs.WithTurnSession(ctx, session)
	turnCtx = sourceregistry.WithContext(turnCtx, a.sources)
	turn, runErr := loom.Run(turnCtx, func(runCtx context.Context, writer loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		result, err := react.Run(runCtx, writer, react.Config{
			Model:             a.model,
			Tools:             a.tools,
			Messages:          messages,
			Reasoning:         loom.Reasoning{Mode: loom.ReasoningModeEnabled},
			MaxSteps:          a.maxSteps,
			MaxToolCalls:      a.maxToolCalls,
			ToolCallLimits:    map[string]uint64{web.ToolNameSearch: 8, web.ToolNameReader: 8, workspacebash.ToolName: 6},
			SoftLandingPrompt: "Stop using tools. Give the best supported answer now, cite only registered [SRC-N] references, and clearly state remaining uncertainty.",
			Purpose:           "deep_research",
		})
		if err != nil {
			return err
		}
		return writer.FinalAnswer(runCtx, result.FinalContent)
	}, loom.RunOptions{
		ConversationID: a.conversationID,
		TurnIndex:      turnNumber - 1,
		History:        append([]loom.Turn(nil), a.history...),
		Input:          loom.UserMessage{Text: userText},
	})
	finishErr := session.Finish(ctx, loomfs.TurnOutcome{Turn: turn, Err: runErr})
	if turn != nil {
		a.history = append(a.history, *turn)
	}
	return turn, errors.Join(runErr, finishErr)
}

// FinalAnswer extracts the final answer text from a completed turn.
func FinalAnswer(turn *loom.Turn) string {
	if turn == nil {
		return ""
	}
	var find func([]loom.Item) string
	find = func(items []loom.Item) string {
		for _, item := range items {
			if item.Kind == loom.ItemKindFinalAnswer {
				return strings.TrimSpace(item.Text)
			}
			if text := find(item.Children); text != "" {
				return text
			}
		}
		return ""
	}
	return find(turn.Items)
}

// SourceCount returns the number of stable references assigned so far.
func (a *App) SourceCount(ctx context.Context) (uint64, error) {
	if a == nil || a.sources == nil {
		return 0, fmt.Errorf("research example: App is not initialized")
	}
	return a.sources.Count(ctx)
}

const researchSystemPrompt = `You are a rigorous deep-research agent.

Workflow:
1. Inspect the conversation workspace before repeating prior work. Use bash on prior_turns.jsonl, queries.jsonl, sources.jsonl, context/turn_contexts.jsonl, and raw/SRC-N.md.
2. Search with a few distinct, high-information queries. Do not issue cosmetic rephrasings.
3. Read primary or otherwise decisive sources. Search snippets are discovery evidence, not full verification.
4. Every web result is assigned a stable SRC-N reference. The same normalized URL keeps the same reference across searches, reads, and later turns.
5. Cite factual claims inline as [SRC-N]. Never invent or renumber a reference.
6. On follow-up turns, reuse relevant prior sources and explain what new evidence changed.
7. Distinguish verified facts, synthesis, and uncertainty. Prefer a concise evidence-backed answer over unsupported breadth.`
