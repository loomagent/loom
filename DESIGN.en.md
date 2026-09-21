[简体中文](DESIGN.md) | **English**

# Loom — Agent framework design document

> This document records loom's positioning, core concepts, and current structure. It
> describes the design **as implemented**; nothing that was never built appears here. The
> code is the authority on names and APIs.

---

## 1. Positioning and goals

### 1.1 What Loom is

- **The core abstraction layer of an agent framework**: wolowork is the first user; it can be extracted into a standalone module later.
- **How a product team writes an agent**: implement a function shaped like a connectrpc handler (`func(ctx, w TurnWriter, history, input) error`) and emit the event stream through a Writer.
- **Persistence / output push / log / otel** are all pluggable Sinks, freely combined on the product side.
- **Lightweight positioning**: no graph orchestration — plain hand-written code plus a unified event output. Similar in role to comparable LLM orchestration frameworks, but lighter.

### 1.2 What Loom is not

- **Not a graph orchestrator**: no `ToolsNode` / `ChatModelNode`-style node abstractions.
- **It does not define business concepts**: Conversation / Chat Mode / user systems and other business-side concepts travel through the metadata passthrough; loom does not interpret them.
- **Not an LLM SDK**: `loom.ChatModel` is the unified LLM abstraction, but concrete providers live in the `providers/` subpackages.
- **It does not force an agent pattern**: ReAct / code orchestration / multi-agent are all expressed under the same API.

---

## 2. Core concepts

### 2.1 Turn — one agent execution

- **Execution boundary**: from the user's question to the final answer (or failure/cancellation).
- **Complete data**: the agent's whole process — reasoning / tool_call / tool_result / step / final_answer.
- **Serializable**: used for full UI rendering and for the next round of conversation context.
- **Two angles**:
  - `loom.Run(ctx, handler, opts)` → returns the resulting `*Turn` data snapshot.
  - Inside the handler you get a `TurnWriter` handle, used for writing.

### 2.2 Item — any node inside a Turn

- Everything is an Item: user_message / reasoning / step / tool_call / tool_result / final_answer.
- **Nested tree**: Items nest through `Children []Item`; there are no ID references.
- **Self-describing path**: every Item has a path such as `turn[0].step[0].step[1].reasoning[2]`.

### 2.3 Step — a nested container

- Expresses **a code-orchestrated sub flow**: a phase / round / subtask / arbitrary scope.
- **Nests to any depth**: `step → step → step → ...`.
- Opened explicitly by the product code: `w.Step(ctx, "label", func(ctx context.Context, s Step) error { ... })`.
- Closes automatically when the closure ends.

### 2.4 Writer — the writing interface

- **The high-level API product code calls**: OOP style, writing through Step / TurnWriter objects.
- **Does not expose ItemRef**: nesting travels through Step objects, so product code never sees one.
- **Closure style**: Step and stream Writers both use closures, and the framework manages Close/Finish/Abort.

### 2.5 Sink — an event downstream

- **Pluggable interface**: Ent / WebSocket / Log / Otel / any product implementation.
- **Does not distinguish "persistence" from "output"**: both are event downstreams and the implementation interprets them freely.
- **Several Sinks go in one `[]Sink`**: the framework fans out to each of them, so product code writes no combinator of its own.

### 2.6 History and context

- The caller loads historical Turns and passes them in through `RunOptions.History`; loom does not care whether history comes from a database, a cache, or memory.
- The conversion that builds context lives in `HistoryToMessages` (see §7.1). The framework never reads or writes history behind your back.

---

## 3. Data model

### 3.1 The path system (entirely 0-based)

```
turn[0]                                      ← the 0th Turn (its index within the conversation)
turn[0].user_message[0]                      ← the user's question (singleton)
turn[0].step[0]                              ← the 0th step
turn[0].step[0].reasoning[0]                 ← the 0th reasoning inside that step
turn[0].step[0].tool_call[0]
turn[0].step[0].tool_result[0]
turn[0].step[0].step[0]                      ← a nested sub flow
turn[0].final_answer[0]                      ← the final answer (a singleton, kept as [0] for uniform shape)

turn[1].user_message[0]                      ← the next round in the same conversation
```

- **0-based**: consistent with Go arrays and the OpenAI Responses API.
- **Each Kind counts independently under its parent**: `step[0].reasoning[0]` coexists with `step[0].tool_call[0]`.
- **The conversation is not part of the path**: `Turn.ConversationID` carries it.

### 3.2 Item structure (one struct plus a Kind field)

```go
type Item struct {
    Kind   ItemKind
    Index  uint64       // the 0-based index of this Kind under the same parent
    Path   string       // "turn[0].step[1].reasoning[0]" (derived)
    Status ItemStatus

    // Kind-specific fields
    Text       string      // user_message / reasoning / final_answer
    Label      string      // step
    ToolName   string      // tool_call / tool_result
    ToolCallID string      // tool_call / tool_result
    Arguments  string      // tool_call
    Output     string      // tool_result
    Error      *ItemError  // a failed item

    // Message provenance (meaningful for a persisted user_message)
    MessageSource  MessageSource
    MessagePurpose MessagePurpose

    // Nesting (non-nil only when Kind=step)
    Children []Item

    // Token accumulation (meaningful for Kind=step)
    Usage Usage

    StartedAt time.Time
    UpdatedAt time.Time
}

type ItemKind string

const (
    ItemKindUserMessage ItemKind = "user_message"
    ItemKindReasoning   ItemKind = "reasoning"
    ItemKindStep        ItemKind = "step"
    ItemKindToolCall    ItemKind = "tool_call"
    ItemKindToolResult  ItemKind = "tool_result"
    ItemKindFinalAnswer ItemKind = "final_answer"
)

type ItemStatus string

const (
    ItemStatusInProgress ItemStatus = "in_progress"
    ItemStatusCompleted  ItemStatus = "completed"
    ItemStatusCancelled  ItemStatus = "cancelled"
    ItemStatusFailed     ItemStatus = "failed"
)

type ItemError struct {
    Code    string // an open enum the product may extend
    Message string
}
```

### 3.3 The Turn snapshot

```go
type Turn struct {
    Index          uint64
    Path           string // "turn[0]"
    ConversationID string
    Items          []Item // the complete item tree, nested

    Status      TurnStatus
    CloseReason *CloseReason // nil = still running
    Usage       Usage
    Metadata    map[string]string

    CreatedAt time.Time
    UpdatedAt time.Time
}
```

### 3.4 TurnStatus / CloseReason

```go
type TurnStatus string

const (
    TurnStatusQueued     TurnStatus = "queued"
    TurnStatusInProgress TurnStatus = "in_progress"
    TurnStatusCompleted  TurnStatus = "completed"
    TurnStatusCancelled  TurnStatus = "cancelled"
    TurnStatusFailed     TurnStatus = "failed"
)

type CloseReason struct {
    Code    CloseCode // the specific reason (a closed enumeration)
    Message string
    Cause   error
}

type CloseCode string

const (
    CloseCodeFinalAnswer     CloseCode = "final_answer"
    CloseCodeUserCancel      CloseCode = "user_cancel"
    CloseCodeTimeout         CloseCode = "timeout"
    CloseCodeHostShutdown    CloseCode = "host_shutdown"
    CloseCodeExternalCancel  CloseCode = "external_cancel"
    CloseCodeAgentError      CloseCode = "agent_error"
    CloseCodeContentFilter   CloseCode = "content_filter"
    CloseCodeOutputTruncated CloseCode = "output_truncated"
    CloseCodeNoFinal         CloseCode = "no_final_answer"
    CloseCodeInternalError   CloseCode = "internal_error"
)
```

Callers that create a Turn row ahead of time, such as a dispatcher, use `queued`.
`loom.Run` starts at `in_progress` and ends in one of the terminal states.

### 3.5 Usage

```go
type Usage struct {
    PromptTokens         uint64
    CompletionTokens     uint64
    CachedTokens         uint64
    ReasoningTokens      uint64
    ReasoningTokensKnown bool // separates "explicitly zero" from "the provider did not report"
    TotalTokens          uint64
}
```

---

## 4. The Writer interface family

### 4.1 Three layers of interfaces

```go
// Writer: the general writing interface (implemented by both the Turn root and Step)
type Writer interface {
    Path() string

    // One-shot writes (immediately Completed)
    WriteReasoning(ctx context.Context, label, text string) error
    WriteToolCall(ctx context.Context, label string, call ToolCall) error
    WriteToolResult(ctx context.Context, label string, result ToolResult) error

    // Streaming writes (closures; the framework finishes or aborts them)
    StreamReasoning(ctx context.Context, label string, fn func(ReasoningStream) error) error

    // Nested sub flow (a closure; the framework closes it)
    Step(ctx context.Context, label string, fn func(ctx context.Context, s Step) error) error
}

// Step: a Writer that is also a nested container (no explicit Close — the closure end closes it)
type Step interface {
    Writer
}

// TurnWriter: a Writer plus what only the Turn root can do (FinalAnswer is written at the root only)
type TurnWriter interface {
    Writer
    FinalAnswer(ctx context.Context, text string) error
    StreamFinalAnswer(ctx context.Context, fn func(FinalAnswerStream) error) error
}
```

### 4.2 The streaming interfaces

The two interfaces have the same method set and one implementation satisfies both. They
are named separately only so a call site says which it means: reasoning, or the final
answer.

```go
type ReasoningStream interface {
    AppendText(ctx context.Context, chunk string) error
    SetFinalText(text string) // optional; overrides the accumulated value
}

type FinalAnswerStream interface {
    AppendText(ctx context.Context, chunk string) error
    SetFinalText(text string)
}
```

### 4.3 Strongly typed parameters

```go
type ToolCall struct {
    Name      string
    ID        string
    Arguments string
}

type ToolResult struct {
    CallID   string
    ToolName string
    Output   string
    Err      *ItemError // non-nil marks the result item Failed
}
```

---

## 5. Handler API and Run

### 5.1 The Handler signature

```go
type Handler func(
    ctx context.Context,
    w TurnWriter,
    history []Turn,
    input UserMessage,
) error

type UserMessage struct {
    Text    string
    Source  MessageSource
    Purpose MessagePurpose
}
```

### 5.2 The Run entry point

```go
func Run(ctx context.Context, h Handler, opts RunOptions) (*Turn, error)

type RunOptions struct {
    Sinks      []Sink
    History    []Turn
    Input      UserMessage

    ConversationID string            // required; Run checks it is non-empty
    TurnIndex      uint64            // 0 = len(History), computed automatically
    Metadata       map[string]string

    OnSinkErr      func(Sink, error) // nil by default, which swallows silently
    StrictSink     bool              // true = any sink failure fails the Turn at once
    CaptureContent bool              // whether prompts, completions and the like go to spans
}
```

There is no Tracer field: OTel goes through the SDK's global TracerProvider, which is the
noop tracer until you configure one.

### 5.3 Deriving CloseReason

```
Priority:
  1. closeReason already set (sealed by FinalAnswer) → keep it
  2. a strict-mode sinkErr                          → {Failed, agent_error}
  3. ctx.DeadlineExceeded                           → {Cancelled, timeout}
  4. ctx.Canceled with a context.Cause              → {Cancelled, user_cancel |
   |                                                    host_shutdown | external_cancel}
  5. handlerErr != nil:
       a cancellation error                       → {Cancelled, ...}
       ErrContentFilter                           → {Failed, content_filter}
       ErrOutputTruncated                         → {Failed, output_truncated}
       anything else                              → {Failed, agent_error}
  6. handlerErr == nil and no FinalAnswer           → {Failed, no_final_answer}
```

### 5.4 Built-in sentinel errors

```go
var (
    ErrUnsupportedCapability = errors.New("loom: provider does not support this request")
    ErrTurnClosed            = errors.New("loom: turn closed")
    ErrHostShutdown          = errors.New("loom: host shutdown")
    ErrExternalCancel        = errors.New("loom: external cancel")
    ErrContentFilter         = errors.New("loom: content filter")
    ErrSensitiveContentRisk  = errors.New("loom: sensitive content risk")
    ErrOutputTruncated       = errors.New("loom: output truncated")
)
```

`ErrContentFilter` means the model already returned `finish_reason=content_filter`.
`ErrSensitiveContentRisk` means the provider refused the call while the request was being
established, usually with an HTTP 400, so the call site has no `ChatResponse`. Providers
map their own official error types onto the latter, and policies above them must not match
a provider's private wording.

### 5.5 Closure failure semantics

| Closure returns | Step / Stream behaviour |
|---|---|
| `nil` | Step Close(Completed) / Stream Finish(the accumulated value or SetFinalText) |
| a cancellation error | Step Close(Cancelled), and the error still propagates |
| any other non-nil error | Step Close(Failed, with Error filled in), and the error propagates |

---

## 6. The Sink family

### 6.1 The Sink interface and its events

```go
type Sink interface {
    ItemStarted(ctx context.Context, ev ItemStartedEvent) error
    ItemDelta(ctx context.Context, ev ItemDeltaEvent) error
    ItemFinished(ctx context.Context, ev ItemFinishedEvent) error
    LLMCalled(ctx context.Context, ev LLMCalledEvent) error
}

type ItemStartedEvent struct {
    TurnIndex uint64
    TurnPath  string
    Item      Item // the complete Item, in its initial state
    Time      time.Time
}

type ItemDeltaEvent struct {
    TurnIndex uint64
    TurnPath  string
    ItemPath  string
    Channel   DeltaChannel // Text / Arguments / Output
    Chunk     string
    Time      time.Time
}

type ItemFinishedEvent struct {
    TurnIndex uint64
    TurnPath  string
    Item      Item // the complete Item in its final state
    Time      time.Time
}

type LLMCalledEvent struct {
    TurnIndex uint64
    TurnPath  string
    StepPath  string // which step it hangs under; empty = the turn root
    Model     string
    Purpose   string
    Usage     Usage
    Time      time.Time
}
```

### 6.2 The Sinks that ship with the framework

```go
// For tests and debugging: collects every event in memory, with per-kind queries and Reset
loom.NewMemorySink() *MemorySink
```

Persistence, frontend push, and metrics are product implementations of `Sink`.

### 6.3 Sink error handling

- Swallow by default: on failure it calls `RunOptions.OnSinkErr(sink, err)` and the main flow continues.
- `StrictSink=true`: any sink failure marks the turn failed at once.
- A failed `AppendText` streaming chunk is always swallowed: losing a delta in the middle does not affect the accumulated value being persisted at the end.

---

## 7. Helpers

### 7.1 History → LLM messages

```go
func HistoryToMessages(history []Turn, input UserMessage) ([]Message, error)
func AppendAssistantTurn(msgs []Message, resp *ChatResponse, results []ToolExecResult) []Message
```

`HistoryToMessages` turns the nested Turn list plus this round's input into
`[]loom.Message` for the model:
- `user_message` → `Role=user`
- `reasoning` → accumulated into the `ReasoningContent` of the first assistant message that follows
- `tool_call` / `tool_result` → paired into `Role=assistant` (carrying ToolCalls) + `Role=tool`
- a history Turn that did not end cleanly returns an error rather than producing dubious context

### 7.2 Running tools

```go
func ExecuteToolCalls(ctx context.Context, w Writer, registry *ToolRegistry, calls []ToolCall) ([]ToolExecResult, error)
func RunToolByName(ctx context.Context, w Writer, label string, registry *ToolRegistry, name string, args any) (string, error)
```

- `ExecuteToolCalls` walks the ToolCalls a model returned: write the tool_call, invoke, write the tool_result. One failure does not stop the rest; `ToolExecResult.Err` records it.
- `RunToolByName` is the code-orchestration case: args is any Go value, which loom marshals, and loom assigns the call ID (`call_0`, `call_1`, ...).

### 7.3 StreamLLMToStep

```go
func StreamLLMToStep(ctx context.Context, w Writer, purpose string, model ChatModel, req ChatRequest) (*ChatResponse, error)
```

Bridges a streaming model response to the Writer:
- a `reasoning_content` chunk is written to a reasoning item as it arrives; the item opens on the first chunk, so no empty item appears
- `content` chunks accumulate into the returned `ChatResponse.Content`
- `tool_call` chunks are assembled into complete ToolCalls and returned without writing a tool_call item; §7.2 writes it just before invoking, which keeps tool_call and tool_result paired

### 7.4 Structured output

```go
func ChatStructuredArgs(ctx context.Context, purpose string, model ChatModel, req ChatRequest, contract *ArgsContract, opts ...StructuredOption) (Args, *ChatResponse, error)
```

The **same** `ArgsContract` that constrains tool arguments constrains what the model
returns: a provider with native `json_schema` support receives the same schema, one that
only supports `json_object` falls back to a JSON object plus a prompt constraint, and the
output is always validated locally against the contract, with output retries.

### 7.5 Synchronous calls and failover

```go
func CallModel(ctx context.Context, purpose string, model ChatModel, req ChatRequest, opts ...CallModelOption) (*ChatResponse, error)
```

The single entry point for synchronous model calls, responsible for tracing and per-call
failover. Providers still own transport retries. `ShouldFailover` and `GetFailoverModel`
decide whether to switch and which model to switch to.

---

## 8. Package layout

```
github.com/loomagent/loom/
  doc.go                        package introduction

  # The declared-argument contract and the schema
  args.go args_arg.go           whole-call validation / typed handles (String/Uint/Enum/Date/...)
  args_contract.go              ArgsContract: build, compile, Decode
  args_error.go                 ToolArgumentError and the model-facing error
  argument_guidance.go          the expected / example arguments summary
  schema.go schema_model.go     the loom.Schema model
  schema_error.go               violation -> model-facing wording
  structured_output.go          ChatStructuredArgs

  # The model abstraction and calling
  llm.go                        ChatModel / Message / ChatRequest/Response / Chunk / Stream
  reasoning_contract.go         the explicit reasoning switch and effort resolution
  call_model.go                 CallModel / per-call failover
  retry.go                      the shared retry schedule (Transient / RateLimit / Permanent)
  tracing.go                    OTel spans
  llmadmission/                 bounded, credential-scoped admission
  attempt_admission.go          AttemptMeta (real supplier quota)
  modelfactory/                 explicit model construction

  # Data model and execution
  turn.go item.go values.go     Turn / Item / UserMessage / ToolCall / ToolResult
  run.go                        Run / RunOptions / CloseReason derivation
  scope.go turn_root.go         the Writer core and the Turn root
  writer.go stream.go stream_impl.go  the Writer family and its streaming implementation
  sink.go sinks.go events.go    the Sink family and MemorySink
  usage_context.go              binding the usage scope

  # Tools and helpers
  tool.go                        Tool / ToolInfo / ToolRegistry / ArgsTool
  tools_run.go                   ExecuteToolCalls / RunToolByName
  messages.go                    HistoryToMessages / AppendAssistantTurn
  stream_llm.go                  StreamLLMToStep
  handlerregistry/              explicit handler registration
  tools/calculator gettime web/...

  # Validation and conformance
  internal/schema/               the schema model itself
  internal/toolcontract/         the validator plus the official JSON Schema Test Suite subset

  # Surrounding frameworks
  modelprobe/                    behavioural model capability probing
  contextpolicy/                 composable context-construction policies
  react/ react/review/           the provider-neutral ReAct runtime and quality gate
  prompttemplate/                placeholder validation and rendering
  sourceregistry/                source deduplication and stable references

  # Providers
  providers/ark deepseek openrouter zhipuai serper unifuncs
  providers/internal/openaicompat/  translation shared by the three OpenAI-wire providers
  providers/internal/probe/         probe serialization shared by four providers

  scripts/check-coverage.sh      the coverage floor
```

---

## 9. Design decisions

| # | Decision | Status |
|---|---|---|
| 1 | No cross-Turn references | done |
| 2 | Entirely 0-based | done |
| 3 | UserMessage is a singleton, stored as an Item with `[0]` | done |
| 4 | FinalAnswer is a singleton, stored as an Item with `[0]`, constrained by the state machine | done |
| 5 | One generic Item struct plus a Kind field; Writer parameters are strongly typed structs | done |
| 6 | The Writer API uses OOP handle objects, so agent code never sees an ItemRef | done |
| 7 | Timeouts travel through ctx only | done |
| 8 | CloseReason is `{Code, Message, Cause}`; Status expresses the broad class | done |
| 9 | Sub flow = nested Step; LLM-driven multi-agent is wrapped as a Tool by product code | done |
| 10 | Handler signature `(ctx, w TurnWriter, history, input)` | done |
| 11 | Three interface layers: Writer / Step / TurnWriter | done |
| 12 | The writer does not write the user_message; the caller persists it first and passes it in | done |
| 13 | Step and Stream\* are all closure style, with the framework managing Close/Finish/Abort | done |
| 14 | One-shot writes stay imperative | done |
| 15 | The streaming interfaces offer SetFinalText | done |
| 16 | `Write*` returns error instead of `*Item` | done |
| 17 | Item Status carries Failed and Cancelled | done |
| 18 | Sink failures are swallowed by default with OnSinkErr; strict mode is configurable | done |
| 19 | The three helpers: HistoryToMessages, tool execution, StreamLLMToStep | done, see §7 |
| 20 | History arrives through `RunOptions.History`; the framework defines no Repository | done |
| 21 | The framework ships only MemorySink; other sinks are product implementations | done |
| 22 | Tool arguments and structured output share one `ArgsContract` | done |
| 23 | Explicit reasoning: every call site declares the switch, and enabling it requires an explicit effort | done |
| 24 | The schema keyword set is closed; decoding rejects anything outside it | done |
| 25 | Tools are declared with typed handles rather than struct tags | done |
| 26 | `ValidateSchema` compiles its schema on every call; nothing caches it, because a schema is a plain value its caller may edit | done |

Dropped: the Note family (reasoning plus a label already carries process information);
CloseDetector (external termination is a cancelled ctx with a cause); `Writer.RunTool`
(`RunToolByName` and `ExecuteToolCalls` cover it); `StreamToolCall` (the caller accumulates
the deltas).

---

## 10. Risks and open questions

### 10.1 Confirmed risks

- **Sink performance**: high-frequency deltas, one frame per token, can become a bottleneck with several sinks. The framework ships no buffering sink; product code implements one when it needs it.
- **Duplication across providers**: the three OpenAI-wire providers share their translation, but `Recv`, `Chat`, and `streamAdapter` are still maintained separately; sharing those needs a set of provider-level hooks whose benefit against their complexity is not yet demonstrated.
- **Ark's wire differences**: Ark uses the Volcengine SDK and does not share `openaicompat`, so its translation layer needs tests of its own (added).

### 10.2 Open questions

- **A multimodal UserMessage**: today it is Text plus Source and Purpose.
- **Cross-Turn references**: a sub-agent referencing a tool result across the main turn would need a redesign; today product code wraps it as a Tool.
- **A faithful reasoning round-trip**: OpenRouter's `reasoning_details` — encrypted or summarised reasoning — has to be echoed back unchanged and in order, and `loom.Message` carries reasoning as one string. Only the plain text form travels today; carrying that structure would need a provider-opaque raw-JSON field on Message.
- **The visibility of rule-based constraints**: whole-call and field-level checks such as `NotBlank` do not appear in the expected-arguments summary, so a model learns about them only by failing. Making them visible would mean the builder carries prose aimed at the model.
