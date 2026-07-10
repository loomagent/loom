# Loom

Loom is a lightweight, event-driven agent runtime for Go. It turns an agent's
execution into a structured event stream that can be consumed by persistence,
realtime UI, logging, and observability backends.

Loom stays deliberately small: agent control flow remains ordinary Go code,
while models, tools, event sinks, and conversation history are replaceable
interfaces.

## Features

- Structured turns, nested steps, reasoning, tool calls, and final answers
- Streaming model and writer APIs
- Fan-out to multiple pluggable `Sink` implementations
- Provider-neutral `ChatModel` abstraction
- OpenTelemetry tracing with content capture disabled by default
- Built-in providers for Ark, DeepSeek, and OpenRouter
- Filesystem-backed context utilities in `loomfs`

## Install

```bash
go get github.com/loomagent/loom
```

Loom currently requires Go 1.26 or newer.

## Quick start

```go
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
		history []loom.Turn,
		input loom.UserMessage,
	) error {
		if err := w.WriteReasoning(ctx, "Plan", "Answer directly"); err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "Hello, "+input.Text)
	}, loom.RunOptions{
		ConversationID: "example",
		Input:          loom.UserMessage{Text: "Loom"},
		Sinks:          []loom.Sink{sink},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(turn.Status)
	fmt.Println(turn.Items[len(turn.Items)-1].Text)
}
```

## Packages

- `github.com/loomagent/loom`: runtime, events, writers, sinks, tools, and model abstractions
- `github.com/loomagent/loom/loomfs`: filesystem-backed context and workspace utilities
- `github.com/loomagent/loom/modelfactory`: storage-independent model construction and configuration loading
- `github.com/loomagent/loom/providers/ark`: Volcengine Ark provider
- `github.com/loomagent/loom/providers/deepseek`: DeepSeek provider
- `github.com/loomagent/loom/providers/openrouter`: OpenRouter provider

The architecture and original design decisions are documented in
[DESIGN.md](DESIGN.md).

## Model factory

`modelfactory` selects providers explicitly and does not infer them from a URL.
It accepts plain Go configuration and has no database or ORM dependency:

```go
model, err := modelfactory.Build(modelfactory.Config{
	Provider: modelfactory.ProviderOpenRouter,
	APIKey:   os.Getenv("OPENROUTER_API_KEY"),
	Model:    "openai/gpt-5",
})
```

Applications that select models by ID can implement
`modelfactory.ConfigLoader` and use `modelfactory.Factory`. A loader may read
from a file, environment variables, a secrets manager, or a database without
coupling Loom to that storage system.

## Project status

Loom is under active development. Until the first stable release, APIs may
change between minor versions. Production users should pin an exact version.

## Development

```bash
go test ./...
go vet ./...
```
