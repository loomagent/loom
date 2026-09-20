<!-- Overview, usage, and integration guidance for the Loom runtime. -->
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
- Built-in providers for Ark, DeepSeek, OpenRouter, and Zhipu AI

## Install

```bash
go get github.com/loomagent/loom
```

Loom currently requires Go 1.27 or newer.

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

## Tools

Tools are declared with `ArgsContract` and `NewArgsTool`. The whole contract —
the public tool name, each argument's type, description, required flag, and
constraints — is written once, and handlers read arguments through typed
handles, so no Go struct, no struct tags, and no string key at a read site are
involved.

Tool names are normally package constants. `ValidateToolName` and
`NewArgsContract` require 1–64 characters matching `^[a-z][a-z0-9_]{0,63}$`;
`ToolRegistry.Register` applies the same validation and rejects duplicate names.

A contract binds the schema, the compiled validator, and the error contract
once, and is immutable and safe for concurrent calls; compiling once avoids
rebuilding the schema for every invocation. Arguments are kept as raw JSON until
a handle reads them, so integers preserve their full 64-bit range instead of
being rounded through float64.

Errors expose `ToolArgumentError` metadata and render a bounded, compact
non-JSON `expected arguments` contract for model self-correction without dumping
the full schema. A validated `example arguments` JSON object is included when
the declared examples form a complete call.

## Declaring tool arguments

Each argument is a typed handle; the handle is both the declaration and the way
the handler reads the value:

```go
func validateDateRange(_ context.Context, from, to string) error {
	if from != "" && to != "" && from > to {
		return loom.InvalidAt("date_to", "date_to (%s) must not precede date_from (%s)", to, from)
	}
	return nil
}

query := loom.String("query").Required().MinLen(1).Desc("Google search query.")
resultType := loom.Enum("type", "search", "news").Desc("Result type; defaults to search.")
dateFrom := loom.Date("date_from").Desc(`Optional lower bound, e.g. "2026-08-17".`)
dateTo := loom.Date("date_to").Desc(`Optional upper bound, e.g. "2026-08-18".`)
limit := loom.Uint("limit").Max(20).Desc("Maximum results to return.")

contract := loom.MustArgsContract("web_search",
	query, resultType, dateFrom, dateTo, limit,
	loom.Cross(dateFrom, dateTo).Using(validateDateRange),
)

tool := loom.NewArgsTool(contract, "Run a Google search.",
	func(ctx context.Context, args loom.Args) (string, error) {
		return search(ctx, query.Get(args), resultType.Get(args), int(limit.Get(args)))
	},
)
```

`Get` returns the argument with the type fixed at declaration, so a read site
has no string key and no type assertion. An optional argument the model omitted
reads back as its zero value, and `Present` distinguishes "omitted" from
"present but empty". Integers are unsigned (`Uint`), so counting arguments
cannot be negative and the schema rejects negatives too. `Date`, `Time`,
`DateTime`, and `UUID` project both a `format` and a matching shape `pattern`,
so providers that ignore `format` still constrain the value. Unknown arguments
are rejected by default.

Field checks take a `FieldValidator[T]`; whole-call checks are declared with
`Cross` / `Cross3` / `Cross4`, which take the typed handles they read:

```go
loom.Cross(dateFrom, dateTo).Using(validateDateRange)
```

The handles make the rule's dependencies part of the contract: every handle is
checked against the declared arguments when the contract is built, the rule is
skipped when none of its arguments are present, and the check receives typed
values rather than `Args`. Prefer a named function over an inline literal, so a
rule can be unit tested directly and is identifiable in stack traces. A rule
that closes over its handles can also point an error at a field without a
string, using `InvalidOn(dateTo, ...)`; `InvalidAt` names the field as a string
for a rule that cannot close over the handle. A field name that is not a
declared argument is treated as an internal error rather than a model-facing
correction request. Validators report model-facing problems with `Invalid`,
`InvalidAt`, or `InvalidOn`; any other error is treated as an internal failure,
and `errors.Join` may report several problems from one validator.

Validation runs in two layers. JSON Schema runs first and enforces type,
presence, enumeration, and the declared range and format constraints; when it
rejects the call, declared validators do not run, because they assume a
well-shaped value. Once the schema passes, every validator runs and its problems
are collected, so the model receives all business-rule violations in a single
turn instead of one per retry. Validators must therefore be cheap, side-effect
free, and safe to run even when another validator has already failed.

## Structured model output

`ChatStructured[T]` derives its JSON Schema with `SchemaFor[T]`, including
supported `validate` constraints such as string lengths. Tool arguments use
`ArgsContract` instead; `SchemaFor` remains for structured output, where the
model returns JSON that is decoded into a Go value. It supplies the same
schema through native `json_schema` or a `json_object` prompt and validates the
response locally. Use `WithStructuredValidator` for additional business rules
or constraints that cannot be represented in JSON Schema.

Every response must be one complete JSON value and pass local schema validation,
regardless of whether the model supports `json_schema`, `json_object`, or only
text output. Markdown fences, surrounding prose, and multiple JSON values are
rejected; JSON whitespace is accepted. No strict-mode option is required:

```go
type Result struct {
    Summary string `json:"summary" validate:"min=1,max=200"`
}

result, response, err := loom.ChatStructured[Result](ctx, "summary", model, request)
```

Invalid JSON or schema violations use the configured output retry limit
(`WithStructuredMaxAttempts`, two attempts by default).

## Packages

- `github.com/loomagent/loom`: runtime, events, writers, sinks, tools, and model abstractions
- `github.com/loomagent/loom/handlerregistry`: concurrent, explicit handler registration
- `github.com/loomagent/loom/modelfactory`: storage-independent model construction and configuration loading
- `github.com/loomagent/loom/modelprobe`: behavioral model capability probing and declaration comparison
- `github.com/loomagent/loom/contextpolicy`: composable context construction and audit decisions
- `github.com/loomagent/loom/react`: provider-neutral ReAct runtime and policy interfaces
- `github.com/loomagent/loom/react/review`: generic ReAct quality gate
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
), modelprobe.Options{
    DeclaredCapabilities: &loom.ModelCapabilities{
        Reasoning: loom.ReasoningSupportAlwaysOn,
        ReasoningEfforts: []loom.ReasoningEffort{"minimal", "low", "medium", "high"},
    },
    DeclarationSource: "reviewed OpenRouter catalog snapshot",
    DeclarationSourceURLs: []string{"https://openrouter.ai/api/v1/models"},
})
```

By default, request errors remain inconclusive and never silently become an
"unsupported" capability. Applications may provide an `ErrorClassifier` when
their provider exposes a reliable unsupported-parameter error classification.
Adapter-local validation errors remain inconclusive even with that classifier.

Object and Schema are independent checks. Schema probes include a fresh random
constraint only in the supplied schema and validate the complete response after
a normal `stop`; truncated or abnormally terminated output is inconclusive.
Reports retain the requested schema and response model for auditing. A passing
sample demonstrates that request and output, not enforcement of every JSON Schema
keyword. `Observed.StructuredOutput` retains the strongest observed success even
when the other check is inconclusive; `Coverage.StructuredOutput` requires both
checks to be conclusive. Inspect individual checks before interpreting the summary.
If `Probe` returns an error after starting, retain its partial report: completed
checks are preserved, and cancellation prevents further experiments.

Probe candidates come only from the administrator's `Options.DeclaredCapabilities`.
`Options.ReasoningEfforts` can explicitly restrict diagnostic candidates; an empty
slice skips them. There is no built-in model catalogue or alias map. Reports record
configured candidates, tested/untested/unresolved efforts, actual wire parameters,
request acceptance, and returned reasoning evidence separately. `Complete` and
`CandidateCoverageComplete` mean the configured candidates were tested. They do
not certify native semantics. Historical reports keep their recorded facts.

`ReasoningEffort` is an open provider-native string type. Administrators must enter
the exact supported values for their provider and model version. Business choices
come exclusively from `ModelCapabilities.ReasoningEfforts`; the Go constants are
convenience values, not a global whitelist. Values are case-sensitive, never
normalized, aliased, ranked, or converted to token budgets. Adapters transmit the
selected value unchanged, including values introduced after an SDK release.

Use `ValidateModelReasoningCapabilities` at save time and `ResolveModelReasoning`
at request time. Every request requires an explicit reasoning switch. Enabled
reasoning requires an explicitly selected effort when the model declares efforts;
disabled reasoning rejects any effort. A confirmed model with no adjustable
strengths uses only the explicit switch. Imported model records that have not been
reviewed must set `ReasoningEffortsUnconfirmed: true`; they cannot enable reasoning.
`OfficialDefaultReasoningEffort` records the reviewed official default. It must
belong to the declared effort list when supplied. It never fills a missing task
choice or changes an existing selection.
Only isolated `ReasoningProbeCapabilities` models intentionally omit controls to
observe provider defaults. Probe results never change business declarations.

## Prompt templates

`prompttemplate` validates that required placeholders occur exactly once before
rendering them. Built-in placeholders cover user input, assistant answers, and
conversation context; callers may also use arbitrary placeholder strings.

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

## Zhipu AI (domestic pay-as-you-go API)

Use `modelfactory.ProviderZhipuAI` (`zhipuai`) or `providers/zhipuai.New`.
The default endpoint is `https://open.bigmodel.cn/api/paas/v4`; this integration
covers the domestic Chat Completions API, not Z.AI or Coding Plan.

```go
model, err := modelfactory.Build(modelfactory.Config{
    Provider: modelfactory.ProviderZhipuAI,
    APIKey: os.Getenv("ZHIPU_API_KEY"),
    Model: "glm-5.3",
    Capabilities: &loom.ModelCapabilities{
        Reasoning: loom.ReasoningSupportAlwaysOn,
        ReasoningEfforts: []loom.ReasoningEffort{
            loom.ReasoningEffortLow, loom.ReasoningEffortHigh, loom.ReasoningEffortMax,
        },
        StructuredOutput: loom.StructuredOutputJSONObject,
    },
})
if err != nil {
    return err
}
response, err := model.Chat(ctx, loom.ChatRequest{
    Messages: []loom.Message{{Role: loom.RoleUser, Content: "Explain your answer briefly."}},
    Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: loom.ReasoningEffortLow},
})
// Handle err and consume response.
```

Capabilities in this example are caller supplied; a nil declaration forwards explicit
controls for probing instead of hardcoding behavior based on a model name.
The adapter transports JSON Object and complete JSON Schema requests in both
Chat and Stream, including schema name, description, constraints, and `strict:true`.
Explicit business capability declarations still gate requests; probes bypass
those gates to observe the endpoint's current behavior. Schema requests are never
downgraded or replaced with prompt instructions. Forced tool selection returns
`loom.ErrUnsupportedCapability`.
It preserves `reasoning_content` across tool calls and enables `tool_stream`
for streaming tool requests. Missing reasoning-token telemetry is not evidence
that thinking is disabled (`Usage.ReasoningTokensKnown` distinguishes missing
telemetry from an explicit zero).

SDK retries are disabled so Loom owns retry budgets. `zhipuai.APIError` retains
HTTP status and business code; balance errors such as 1113 fail immediately,
1302 uses bounded rate-limit backoff, and 1305 uses finite transient retries.
See [Zhipu API documentation](https://docs.bigmodel.cn/cn/api/introduction) and
[GLM-5.3](https://docs.bigmodel.cn/cn/guide/models/text/glm-5.3).

Reasoning capabilities use three states: `none`, `always_on`, and `toggleable`.
Legacy `toggleable_default_on/off` values remain accepted and normalize through
`ReasoningSupport.Canonical()`. Application requests explicitly select reasoning;
when declared effort levels exist, enabling reasoning also requires an explicit
supported effort. Probe reports retain the server-default observation separately.
