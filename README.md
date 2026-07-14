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

Runnable ReAct examples are available in [`examples/react`](examples/react):

- a minimal calculator and time tool loop;
- one-turn web deep research with stable citations;
- an interactive multi-turn research conversation that reuses `SRC-N`
  references and workspace evidence across turns.

## Struct-derived tool schemas

Define tool arguments once as a Go struct. `SchemaFor` and `MustSchemaFor`
derive property types, names, required fields, descriptions, and nested shapes:

```go
type calculatorRequest struct {
	Expression string `json:"expression" jsonschema:"Mathematical expression to evaluate." validate:"min=1,notblank" example:"(2 + 3) * 4"`
	Precision  int    `json:"precision,omitempty" jsonschema:"Optional decimal precision." validate:"omitempty,min=0,max=12"`
}

contract := loom.MustToolContract[calculatorRequest]("calculator")
tool := loom.NewTool(
	contract,
	"Evaluate a mathematical expression.",
	func(ctx context.Context, request calculatorRequest) (string, error) {
		// ...
	},
)
```

Fields without `omitempty` or `omitzero` are required. Validation tags are
executed by `go-playground/validator`, the validator used by Gin. Rules with a
direct schema equivalent—including `required`, size and comparison rules,
`eq`/`ne`, `oneof`, string prefix/suffix/containment rules, `unique`, `dive`,
and Loom's `notblank`—are also projected into JSON Schema. Cross-field and
custom rules remain runtime-only. `DecodeToolArguments` checks the incoming
JSON against the generated schema and validates the decoded struct, so model
guidance and server-side enforcement stay in sync. Derived object schemas
reject unknown properties by default. An `example` tag is
projected into JSON Schema. When every required argument has an example, Loom
also assembles a complete example call and accepts it only after both Schema
and struct validation succeed.

`ToolContract` binds the tool name, generated Schema, compiled validator, and
error contract once. `NewTool` then passes already validated arguments to
the handler. Contracts are immutable and safe for concurrent calls; compiling
once avoids rebuilding and resolving the Schema for every invocation. Argument
decoding preserves the full `int64`/`uint64` range instead of routing integers
through `float64`.

`NewTool` is the only public tool constructor. Tools without parameters use
`ToolContract[loom.NoArguments]` and accept the empty JSON object `{}`; raw JSON
handlers remain an internal implementation detail.

Errors expose `ToolArgumentError` metadata and render a bounded, compact
non-JSON `expected arguments` contract for model self-correction without
dumping the full schema. A validated `example arguments` JSON object is included
when the struct declares a complete example.

## Packages

- `github.com/loomagent/loom`: runtime, events, writers, sinks, tools, and model abstractions
- `github.com/loomagent/loom/handlerregistry`: concurrent, explicit handler registration
- `github.com/loomagent/loom/loomfs`: filesystem-backed context and workspace utilities
- `github.com/loomagent/loom/modelfactory`: storage-independent model construction and configuration loading
- `github.com/loomagent/loom/modelprobe`: behavioral model capability probing and declaration comparison
- `github.com/loomagent/loom/contextpolicy`: composable context construction and audit decisions
- `github.com/loomagent/loom/react`: provider-neutral ReAct runtime and policy interfaces
- `github.com/loomagent/loom/react/review`: generic ReAct quality gate
- `github.com/loomagent/loom/proreportbench`: offline report-agent trace and artifact evaluation
- `github.com/loomagent/loom/prompttemplate`: explicit prompt placeholder validation and rendering
- `github.com/loomagent/loom/sourceregistry`: storage-neutral source deduplication and stable citation IDs
- `github.com/loomagent/loom/sourceregistry/sourceregistrytest`: reusable Store conformance suite
- `github.com/loomagent/loom/providers/ark`: Volcengine Ark provider
- `github.com/loomagent/loom/providers/deepseek`: DeepSeek provider
- `github.com/loomagent/loom/providers/openrouter`: OpenRouter provider
- `github.com/loomagent/loom/providers/serper`: Serper web-search provider
- `github.com/loomagent/loom/providers/unifuncs`: Unifuncs document-reader provider
- `github.com/loomagent/loom/tools/web/sourcedate`: provider-neutral publication-date extraction
- `github.com/loomagent/loom/tools/web`: provider-neutral search and reader contracts
- `github.com/loomagent/loom/tools/calculator`: sandboxed Starlark calculator
- `github.com/loomagent/loom/tools/gettime`: fixed Beijing-time tool
- `github.com/loomagent/loom/tools/workspace`: in-memory workspace backend and file tools
- `github.com/loomagent/loom/tools/workspacebash`: validated, read-only shell tool contract
- `github.com/loomagent/loom/tools/workspacebash/gobash`: pure-Go workspace shell runner

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

## Model capability probing

`modelprobe` observes real API behavior instead of trusting configuration. It
tests default and explicit reasoning behavior, accepted reasoning-effort values,
and native JSON object and JSON Schema output. Reports distinguish positive,
negative, and operationally inconclusive checks, carry a versioned JSON schema,
and can be compared with declared `loom.ModelCapabilities` without a database:

```go
base := modelfactory.Config{
	Provider: modelfactory.ProviderOpenRouter,
	APIKey:   os.Getenv("OPENROUTER_API_KEY"),
	Model:    "openai/gpt-5",
}

report, err := modelprobe.Probe(ctx, modelprobe.BuilderFunc(
	func(_ context.Context, capabilities loom.ModelCapabilities) (loom.ChatModel, error) {
		cfg := base
		cfg.Capabilities = &capabilities
		return modelfactory.Build(cfg)
	},
), modelprobe.Options{})
```

By default, request errors remain inconclusive and never silently become an
"unsupported" capability. Applications may provide an `ErrorClassifier` when
their provider exposes a reliable unsupported-parameter error classification.

## Workspace tools

The workspace package provides `ls`, `read_file`, `write_file`, and `edit_file`
as Loom tools over a small storage interface. Its in-memory and local filesystem
backends enforce read-before-write for existing files:

```go
backend := workspace.NewInMemoryBackend()
tools := loom.NewToolRegistry()
if err := workspace.RegisterAll(tools, backend); err != nil {
	return err
}
```

Applications can implement `workspace.Backend` to add their own persistent
storage without introducing an ORM dependency into Loom.

For a persistent local workspace, pass the root explicitly. The backend confines
all access beneath that directory, including symbolic-link resolution:

```go
backend, err := workspace.OpenLocalBackend("./agent-workspace")
if err != nil {
	return err
}
defer backend.Close()
```

## Prompt templates

`prompttemplate` validates that required placeholders occur exactly once before
rendering them. Built-in placeholders cover user input, assistant answers, and
conversation context; callers may also use arbitrary placeholder strings.

## Read-only workspace shell

`workspacebash` exposes a constrained shell tool for agents, while `gobash`
executes its allowlisted commands entirely in process. Filesystem access is
confined to an `os.Root`; writes, host command fallback, command substitution,
background jobs, and non-allowlisted commands are rejected.

```go
runner, err := gobash.New(gobash.Options{WorkspaceDir: workspaceDir})
if err != nil {
	return err
}
defer runner.Close()

bashTool := workspacebash.NewTool(workspacebash.ToolOptions{
	Runner:      runner,
	Description: "Search and inspect files in the read-only workspace.",
})
```

The built-in command set includes `cat`, `grep`, `jq`, `find`, `ls`, `sed`,
`head`, `tail`, `sort`, `uniq`, `xargs`, and other read-only text utilities.

## Source dates

`tools/web/sourcedate` conservatively extracts publication dates from the top
of Markdown documents. It recognizes labelled and standalone English, ISO, and
Chinese date formats while rejecting implausibly old or future dates. Results
include the original text, parsed UTC date, evidence source, and confidence.

The package only parses document content. Search-provider metadata and source
registry policies intentionally remain outside this package.

## Source registry

`sourceregistry` assigns stable `SRC-N` references within a conversation or
other namespace. It normalizes URLs, deduplicates within and across batches,
allocates new references consecutively in first-seen order, preserves metadata
provenance, and upgrades full-content availability monotonically:

```go
store := sourceregistry.NewMemoryStore()
registry, err := sourceregistry.New("conversation-123", store)
if err != nil {
	return err
}

refs, err := registry.EnsureBatch(ctx, []sourceregistry.Input{
	{URL: "https://example.com/report", Origin: "web_search", Title: "Report"},
	{URL: "https://example.com/report#results", Origin: "web_reader", HasContent: true},
})
```

The two observations above share one sequence and content path. `Created` is
true only on the first input, so it can be counted without double-counting.
Applications can replace `MemoryStore` with a transactional database Store.
The Store contract requires per-namespace linearizability, unique URL and
sequence keys, contiguous allocation, ordered results, and all-or-nothing batch
commits. A custom URL normalizer can implement product-specific rules such as
tracking-parameter removal.

Database adapters can run the same conformance suite used by `MemoryStore`:

```go
func TestStoreContract(t *testing.T) {
	sourceregistrytest.TestStore(t, func(t *testing.T) sourceregistry.Store {
		return newTestStore(t)
	})
}
```

The suite checks empty batches, ordered contiguous allocation, metadata merging,
namespace isolation, returned-value aliasing, canceled transactions, and
linearizable concurrent registration of both distinct and identical sources.

## ReAct runtime

`react.Run` handles streaming model calls, tool execution, per-tool and total
budgets, finish reasons, and a final tool-free soft landing. Applications can
extend it without forking the loop through three small policy interfaces:

- `StepPolicy` adjusts context, visible tools, and tool choice before a call.
- `AfterToolsPolicy` reviews results, changes context, or stops the loop.
- `FinishPolicy` accepts or rejects a model's attempt to finish.

`contextpolicy.ReactStepPolicy` adapts composable context builders to the loop.
`react/review.Policy` supplies a stateful quality gate while leaving the actual
reviewer, criteria, and instructions to the application.

## Report-agent evaluation

`proreportbench` performs offline comparisons between report-agent traces and
reference marker traces. It normalizes Loom turns, item arrays, and generic
JSON item trees into signals, checks required ordering and forbidden events,
builds batch comparison reports, and extracts metrics from stable research
artifacts. It has no database, model, tool, or network dependency.

## Web tools

The `tools/web` package defines normalized `WebSearcher` and `WebReader` interfaces
plus Loom tool wrappers. Provider implementations own network access, caching,
authentication, and retries. The public tool layer does not assign citation
IDs, persist documents, or depend on a search vendor.

Serper and Unifuncs are available as optional provider implementations. Their
clients directly satisfy the provider-neutral interfaces:

```go
searchTool, err := web.NewSearchTool(
	serper.New(os.Getenv("SERPER_API_KEY")),
	web.SearchToolOptions{},
)
if err != nil {
	return err
}

readerTool, err := web.NewReaderTool(
	unifuncs.New(os.Getenv("UNIFUNCS_API_KEY")),
	web.ReaderToolOptions{},
)
if err != nil {
	return err
}
```

The Unifuncs provider includes request throttling, bounded retries for transient
failures, `Retry-After` handling, and publication-date extraction. The Serper
provider preserves vendor date and result-position fields as result metadata.

## Built-in utility tools

`tools/calculator` evaluates expressions in a restricted Starlark environment
and uses ordinary JSON request/response structs. `tools/gettime` intentionally
returns a fixed Asia/Shanghai local time alongside UTC; models cannot override
the timezone through tool arguments.

## Project status

Loom is under active development. Until the first stable release, APIs may
change between minor versions. Production users should pin an exact version.

## Development

```bash
go test ./...
go vet ./...
```
