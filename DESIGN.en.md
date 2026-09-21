[简体中文](DESIGN.md) | **English**

# Loom — Agent framework design document

> This document records loom's core design decisions and implementation roadmap.
> Loom is an agent framework: its goal is to weave the output of heterogeneous
> agent implementations (hand-written orchestration / any LLM SDK) into a
> unified event stream, fanned out to multiple downstreams
> (database / WebSocket / log / otel).

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
- **Not an LLM SDK**: `loom.ChatModel` is the unified LLM abstraction, but concrete providers (DeepSeek/OpenAI/...) live in the `providers/` subpackages and can become independent libraries later.
- **It does not force an agent pattern**: ReAct / code orchestration / multi-agent are all expressed under the same API.

---

## 2. Core concepts

### 2.1 Turn — one agent execution

- **Execution boundary**: from the user's question to the final answer (or failure/cancellation).
- **Complete data**: the user's question plus the agent's whole process (reasoning / tool_call / tool_result / step / final_answer).
- **Serializable**: used for full UI rendering and for the next round of conversation context.
- **Two angles**:
  - `loom.Run(ctx, handler, opts)` → returns the resulting `*Turn` data snapshot.
  - Inside the handler you get a `TurnWriter` handle, used for writing.

### 2.2 Item — any node inside a Turn

- Everything is an Item: user_message / reasoning / step / note / tool_call / tool_result / final_answer.
- **Nested tree**: Items nest through `Children []Item`; there are no ID references.
- **Self-describing path**: every Item has a path such as `turn[0].step[0].step[1].reasoning[2]`.

### 2.3 Step — a nested container

- Expresses **a code-orchestrated sub flow**: a phase / round / subtask / arbitrary scope.
- **Nests to any depth**: `step → step → step → ...`.
- Opened explicitly by the product code: `w.Step(ctx, "label", func(s Step) error { ... })`.
- Closes automatically when the closure ends.

### 2.4 Writer — the writing interface

- **The high-level API product code calls**: OOP style, writing through Step / TurnWriter objects.
- **Does not expose ItemRef**: nesting travels through Step objects, so product code never sees one.
- **Closure style**: Step and stream Writers both use closures, and the framework manages Close/Finish/Abort.

### 2.5 Sink — an event downstream

- **Pluggable interface**: Ent / WebSocket / Log / Otel / any product implementation.
- **Does not distinguish "persistence" from "output"**: both are event downstreams and the implementation interprets them freely.
- **Multiple Sinks combine through TeeSink**: fan-out to several destinations.

### 2.6 Repository — history recall (read)

- Loads history across Turns: `LoadHistory(convID) []Turn`.
- Separate from Sink: Sink writes, Repository reads.
- Implemented by the product side (from a DB / memory / anything).

### 2.7 CloseDetector — external termination detection (optional)

- Detects that a Turn was cancelled/failed externally (triggered by a dispatcher / StopGeneration).
- Needed only by implementations that have an authoritative state source (such as EntDetector querying the DB); Log/Memory do not need it.

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
turn[0].step[0].step[0].note[0]
turn[0].final_answer[0]                      ← the final answer (a singleton, kept as [0] for uniform shape)

turn[1].user_message[0]                      ← the next round in the same conversation
```

- **0-based**: consistent with Go arrays and the OpenAI Responses API.
- **Each Kind counts independently under its parent**: `step[0].reasoning[0]` coexists with `step[0].tool_call[0]`.
- **The conversation is not part of the path**: it travels in the metadata.

### 3.2 Item structure (one struct plus a Kind field)

```go
type Item struct {
    Kind   ItemKind
    Index  uint64       // the 0-based index of this Kind under the same parent
    Path   string       // "turn[0].step[1].reasoning[0]" (derived)
    Status ItemStatus

    // Kind-specific fields
    Text       string      // user_message / reasoning / final_answer / note
    Label      string      // step
    NoteKind   NoteKind    // note
    ToolName   string      // tool_call / tool_result
    ToolCallID string      // tool_call / tool_result
    Arguments  string      // tool_call
    Output     string      // tool_result
    Error      *ItemError  // a failed item

    // Nesting (non-nil only when Kind=step)
    Children []Item

    StartedAt time.Time
    UpdatedAt time.Time
}

type ItemKind string

const (
    ItemKindUserMessage ItemKind = "user_message"
    ItemKindReasoning   ItemKind = "reasoning"
    ItemKindNote        ItemKind = "note"
    ItemKindStep        ItemKind = "step"
    ItemKindToolCall    ItemKind = "tool_call"
    ItemKindToolResult  ItemKind = "tool_result"
    ItemKindFinalAnswer ItemKind = "final_answer"
)

type NoteKind string

const (
    NoteSummary    NoteKind = "summary"    // a summary of a phase's output
    NoteDiagnostic NoteKind = "diagnostic" // an anomaly or diagnostic message
    NoteDraft      NoteKind = "draft"      // a streaming draft
    NoteRevision   NoteKind = "revision"   // a revision or review
)

type ItemStatus string

const (
    ItemStatusInProgress ItemStatus = "in_progress"
    ItemStatusCompleted  ItemStatus = "completed"
    ItemStatusCancelled  ItemStatus = "cancelled"
    ItemStatusFailed     ItemStatus = "failed"
)

type ItemError struct {
    Code    string
    Message string
}
```

### 3.3 The Turn snapshot

```go
type Turn struct {
    Index uint64
    Path  string // "turn[0]"

    Items []Item // the complete item list (a nested tree)

    Status      TurnStatus
    CloseReason CloseReason
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
    Code    CloseCode // the detailed reason (a closed enumeration)
    Message string
    Cause   error
}

type CloseCode string

// Built-in Code constants
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

### 3.5 Usage (as defined by the LLM abstraction layer)

```go
type Usage struct {
    PromptTokens     uint64
    CompletionTokens uint64
    CachedTokens     uint64
    ReasoningTokens  uint64
    TotalTokens      uint64
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
    WriteReasoning(ctx context.Context, displayName, text string) error
    WriteNote(ctx context.Context, displayName, text string, kind NoteKind) error
    WriteToolCall(ctx context.Context, displayName string, call ToolCall) error
    WriteToolResult(ctx context.Context, displayName string, result ToolResult) error

    // Streaming writes (closures; the framework finishes or aborts them)
    StreamReasoning(ctx context.Context, displayName string, fn func(ReasoningStream) error) error
    StreamNote(ctx context.Context, displayName string, kind NoteKind, fn func(NoteStream) error) error

    // Nested sub flow (a closure; the framework closes it)
    Step(ctx context.Context, label string, fn func(Step) error) error
}

// Step: Writer plus Step's own attributes (no explicit Close — the closure end closes it)
type Step interface {
    Writer
}

// TurnWriter: Writer plus what only the Turn root can do (FinalAnswer is written at the root only)
type TurnWriter interface {
    Writer
    FinalAnswer(ctx context.Context, text string) error
    StreamFinalAnswer(ctx context.Context, fn func(FinalAnswerStream) error) error
}
```

### 4.2 The streaming interfaces

```go
type ReasoningStream interface {
    AppendText(ctx context.Context, chunk string) error
    SetFinalText(text string) // optional; overrides the accumulated value
}

type NoteStream interface {
    AppendText(ctx context.Context, chunk string) error
    SetFinalText(text string)
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
    CallID    string
    Arguments string
}

type ToolResult struct {
    CallID string
    Output string
    Err    *ToolError
}

type ToolError struct {
    Code    string
    Message string
}
```

---

## 5. Handler API

### 5.1 The Handler signature

```go
type Handler func(
    ctx context.Context,
    w TurnWriter,
    history []Turn,
    input UserMessage,
) error

type UserMessage struct {
    Text string
    // reserved for multimodal extensions (Images / Files and so on)
}
```

### 5.2 The Run entry point

```go
func Run(ctx context.Context, h Handler, opts RunOptions) (*Turn, error)

type RunOptions struct {
    Sinks      []Sink
    Repository Repository      // optional; history recall
    Detector   CloseDetector   // optional; external termination detection
    Tracer     trace.Tracer    // optional; nil = otel.Tracer("loom")

    History []Turn
    Input   UserMessage

    TurnIndex uint64 // 0 = len(History), computed automatically
    Metadata  map[string]string

    OnSinkErr func(Sink, error) // sink failure callback; by default logs a warning and continues
    StrictSink bool             // true = any sink failure fails the Turn immediately
}
```

### 5.3 The state machine that decides the outcome

```
err = handler(ctx, w, history, input)

if errors.Is(err, context.DeadlineExceeded):
    Status=cancelled, CloseReason{Code: "timeout", Cause: err}
if errors.Is(err, context.Canceled):
    Status=cancelled, CloseReason{Code: "user_cancel", Cause: err}
if errors.Is(err, ErrContentFilter):
    Status=failed, CloseReason{Code: "content_filter", Cause: err}
if errors.Is(err, ErrLength):
    Status=failed, CloseReason{Code: "output_truncated", Cause: err}
if err != nil:
    Status=failed, CloseReason{Code: "agent_error", Cause: err}
if !w.HasFinalAnswer():
    Status=failed, CloseReason{Code: "no_final_answer"}
else:
    Status=completed, CloseReason{Code: "final_answer"}
```

### 5.4 Built-in sentinel errors

```go
var (
    ErrContentFilter   = errors.New("loom: content filter")
    ErrLength          = errors.New("loom: length limit")
    ErrTurnClosed      = errors.New("loom: turn closed")
    ErrStepIncomplete  = errors.New("loom: step incomplete")
)
```

Product code:

```go
return loom.ErrStepIncomplete       // the step is marked Incomplete; not an error (does not bubble)
return errors.New("real error")     // the step is marked Failed and the error bubbles
return nil                          // the step is marked Succeeded
```

### 5.5 Closure failure semantics

| Closure returns | Step / Stream behaviour |
|---|---|
| `nil` | Step Close(Succeeded) / Stream Finish(the accumulated value or SetFinalText) |
| `ErrStepIncomplete` (sentinel) | Step Close(Incomplete); **the error does not bubble** (the step absorbs it) |
| Any other non-nil error | Step Close(Failed) / Stream Abort(err), and the error bubbles |

---

## 6. The Sink family

### 6.1 The Sink interface

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
    Item      Item // the complete Item, with status=in_progress
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
    Item      Item // the final Item, including Status and the complete payload
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

type DeltaChannel string

const (
    DeltaChannelText      DeltaChannel = "text"
    DeltaChannelArguments DeltaChannel = "arguments"
    DeltaChannelOutput    DeltaChannel = "output"
)
```

### 6.2 The Repository interface

```go
type Repository interface {
    LoadHistory(ctx context.Context, conversationID string) ([]Turn, error)
    LoadTurn(ctx context.Context, conversationID string, index uint64) (*Turn, error)
}
```

### 6.3 The CloseDetector interface (optional)

```go
type CloseDetector interface {
    CheckClose(ctx context.Context) (CloseReason, error)
}
```

### 6.4 Sinks that ship with the framework

```go
// For tests: collects every event in memory and offers assertion helpers
loom.NewMemorySink() *MemorySink

// For debugging: structured logging through zap
loom.NewLogSink(logger *zap.Logger) Sink

// Multi-writer: concurrent fan-out to several sinks
loom.TeeSink(sinks ...Sink) Sink

// Filtering: routes by predicate
loom.FilterSink(predicate func(any) bool, inner Sink) Sink

// Buffering (relieving pressure from high-frequency deltas):
loom.BufferedSink(inner Sink, batchSize int, flushInterval time.Duration) Sink
```

### 6.5 Sink error handling

- Swallow by default: on failure it calls `RunOptions.OnSinkErr(sink, err)` and the main flow continues.
- `StrictSink=true`: any sink failure immediately marks the turn `Failed`.
- A failed `AppendText` streaming chunk is always swallowed: losing a delta in the middle does not affect the accumulated value being persisted at the end.

---

## 7. High-value helpers (built together with v1)

### 7.1 History → LLM messages

```go
func HistoryToMessages(history []Turn, input UserMessage) []Message
```

Turns the nested Turn list plus this round's input into `[]loom.Message` for the LLM:
- `user_message` → `Role=user`
- `final_answer` → `Role=assistant`
- `tool_call` + `tool_result` → paired into `Role=assistant` (carrying ToolCalls) + `Role=tool` (carrying ToolCallID)

### 7.2 RunTool, merging tool_call and result

```go
// A helper on Step / Writer: write the tool_call → invoke the tool → write the tool_result
func (w Writer) RunTool(ctx context.Context, registry *ToolRegistry, name, argsJSON string) (string, error)
```

### 7.3 StreamLLMToStep

```go
// Bridges an LLM stream to Step writes automatically:
// - reasoning_content → StreamReasoning
// - content → StreamNote (or a custom kind)
// - tool_call → WriteToolCall
type StreamLLMResult struct {
    FinalText        string
    ToolCalls        []ToolCall
    Usage            Usage
    FinishReason     FinishReason
    ReasoningContent string
}

func StreamLLMToStep(ctx context.Context, w Writer, purpose string, model ChatModel, req ChatRequest, opts ...StreamOption) (*StreamLLMResult, error)
```

It replaces the react executor's current StreamConverter and shrinks the ReAct executor to about 30 lines.

### 7.4 ChatStructured

```go
func ChatStructured[T any](ctx context.Context, purpose string, model ChatModel, req ChatRequest, opts ...StructuredChatOption[T]) (T, *ChatResponse, error)
```

Generates the JSON Schema from a Go struct automatically, picks the provider's native `json_schema` / `json_object` / prompt-only mode from `model.Capabilities()`, and always parses and validates against the schema locally.
When the output does not satisfy the structure, the failure reason is written back into the next request and the call is retried; product code can add domain validation through `WithStructuredValidator`.

### 7.5 CallModel / failover

```go
func CallModel(ctx context.Context, purpose string, model ChatModel, req ChatRequest, opts ...CallModelOption) (*ChatResponse, error)
```

The unified entry point for synchronous model calls. Providers remain responsible for transport retries; `CallModel` handles tracing and per-call failover.
Failover follows Eino's shape: `ShouldFailover(ctx, attempt)` decides whether to switch, and `GetFailoverModel(ctx, attempt)` returns the fallback model. `FailoverAttempt.Request` exposes the request actually being made so the decision can use it; rewriting the request is deliberately not part of the failover policy.

---

## 8. The `loom/loomtest` test toolkit

```go
package loomtest

// MemorySink collects every event and offers assertion helpers
type MemorySink struct { ... }

func NewMemorySink() *MemorySink
func (s *MemorySink) Events() []Event
func (s *MemorySink) Items() []Item

// MockChatModel replays a preset sequence of responses
type MockChatModel struct { ... }

func NewMockChatModel(responses ...MockResponse) *MockChatModel

// Assertions
func AssertEvents(t *testing.T, sink *MemorySink, matchers []EventMatcher)
func AssertTurnStatus(t *testing.T, turn *Turn, status TurnStatus)
```

---

## 9. Decision list (all 22 items)

| # | Decision | Implemented |
|---|---|---|
| 1 | No cross-Turn references |  |
| 2 | Entirely 0-based; change the DB schema accordingly |  |
| 3 | UserMessage is a singleton, stored as an Item with `[0]`, constrained by the state machine |  |
| 4 | FinalAnswer is a singleton, stored as an Item with `[0]`, constrained by the state machine |  |
| 5 | One generic Item struct plus a Kind field; Writer parameters are strongly typed structs |  |
| 6 | The Writer API uses OOP handle objects, so agent code never sees an ItemRef |  |
| 7 | Timeouts travel through ctx only |  |
| 8 | CloseReason is `{Code, Message, Cause}`; Status expresses the broad completed/cancelled/failed classes |  |
| 9 | Sub flow = nested Step (the code-orchestration base); LLM-driven multi-agent is wrapped as a Tool by the product code |  |
| 10 | Handler signature `(ctx, w TurnWriter, history, input)`; the Writer is passed explicitly |  |
| 11 | Three interface layers: Writer / Step / TurnWriter |  |
| 12 | Run writes the UserMessage automatically; the Writer does not expose WriteUserMessage |  |
| 13 | Step and Stream\* are all closure style; the framework manages Close/Finish/Abort |  |
| 14 | One-shot writes stay imperative |  |
| 15 | The streaming interfaces offer SetFinalText |  |
| 16 | `Write*` returns error instead of `*Item` |  |
| 17 | Item Status gains a Failed state; a closure returning the ErrStepIncomplete sentinel does not bubble |  |
| 18 | Sink failures are swallowed by default with an OnSinkErr callback; strict mode is configurable |  |
| 19 | The three helpers HistoryToMessages / RunTool / StreamLLMToStep are part of v1 |  |
| 20 | A `loom/loomtest` subpackage (MemorySink / MockChatModel / assertions) |  |
| 21 | TeeSink / LogSink built in |  |
| 22 | UserMessage stays a struct (with a single Text field), reserved for multimodal input |  |

---

## 10. Implementation roadmap

Staged in dependency order; each stage ends with a passing build/vet plus simple unit tests.

### Phase 0 ✅ (done)

- `pkg/loom/llm.go` — LLM abstraction (ChatModel/Message/Stream)
- `pkg/loom/tool.go` — tool abstraction
- `pkg/loom/providers/deepseek/` — the DeepSeek provider

### Phase 1: core data types

**Goal**: define the dependency-free data structs Turn / Item / Path / Status / CloseReason.

Files:
- `pkg/loom/turn.go` — Turn / TurnStatus / CloseReason
- `pkg/loom/item.go` — Item / ItemKind / ItemStatus / NoteKind / ItemError
- `pkg/loom/path.go` — path assembly helpers (optional; inlining is fine for simple cases)

Verification: pure data structures; a passing build is enough.

### Phase 2: the Sink interfaces and event types

**Goal**: define the Sink / Repository / CloseDetector interfaces and the event structs.

Files:
- `pkg/loom/sink.go` — Sink / Repository / CloseDetector interfaces
- `pkg/loom/events.go` — ItemStartedEvent / ItemDeltaEvent / ItemFinishedEvent / LLMCalledEvent / DeltaChannel

Verification: interface definitions; a passing build.

### Phase 3: the Writer interfaces

**Goal**: define the Writer / Step / TurnWriter / Stream\* interfaces.

Files:
- `pkg/loom/writer.go` — Writer / Step / TurnWriter interfaces
- `pkg/loom/stream.go` — ReasoningStream / NoteStream / FinalAnswerStream interfaces
- `pkg/loom/values.go` — the strongly typed UserMessage / ToolCall / ToolResult / ToolError structs

Verification: interface definitions; a passing build.

### Phase 4: the Writer core implementation

**Goal**: implement the Writer / Step / TurnWriter / Stream\* interfaces, **closure management included**.

Key internals:
- an ID/path generator (per Turn, with 0-based counters per parent and kind)
- the state machine (InProgress → Completed/Incomplete/Failed)
- concurrency detection (a Writer within one step may not be used concurrently; panic)
- the sealed state (after WriteFinalAnswer, `Write*` is rejected)

Files:
- `pkg/loom/internal/writerimpl/writer.go` — the core
- `pkg/loom/internal/writerimpl/step.go`
- `pkg/loom/internal/writerimpl/stream.go`

Verification: unit tests covering every Close/Finish/Abort path of the closures.

### Phase 5: default Sink implementations

**Goal**: the MemorySink / LogSink / TeeSink built-in sinks.

Files:
- `pkg/loom/sinks/memory/sink.go` — MemorySink plus Events()/Items() queries
- `pkg/loom/sinks/log/sink.go` — LogSink (zap)
- `pkg/loom/sinks/tee/sink.go` — TeeSink

Verification: a unit test per sink.

### Phase 6: Run + Handler + the error state machine

**Goal**: the `loom.Run` entry point, the Handler signature, error handling, and CloseReason derivation.

Files:
- `pkg/loom/run.go` — Run / RunOptions / Handler / the error derivation
- `pkg/loom/errors.go` — sentinels (ErrContentFilter / ErrLength / ErrStepIncomplete / ErrTurnClosed)

Verification:
- normal completion (WriteFinalAnswer) → Completed
- returning an error → Failed (with the various codes)
- ctx cancel/timeout → Cancelled
- a programming-error panic bubbles straight out; `loom.Run` does not recover it
- no WriteFinalAnswer → Failed("no_final_answer")

### Phase 7: built-in Otel

**Goal**: the Writer core opens a span for every Item / Step / LLMCall and attaches ctx baggage.

Implementation notes:
- on ItemStarted, `tracer.Start()` a span and record it in `spans[ItemPath]`
- on ItemFinished, `End()` it
- ParentRef nests the trace tree automatically
- LLMCalled uses the standard `gen_ai.*` attributes

Files:
- `pkg/loom/otel.go` (wired into the core)

Verification: run an agent and see a complete trace tree in an OTLP-compatible backend.

### Phase 8: helpers

**Goal**: the three high-value helpers HistoryToMessages / RunTool / StreamLLMToStep.

Files:
- `pkg/loom/messages.go` — HistoryToMessages
- `pkg/loom/tools_run.go` — Writer.RunTool / ToolRegistry helpers
- `pkg/loom/stream_llm.go` — StreamLLMToStep

Verification: convert wolowork's existing react executor to the helper-driven version and compare line counts.

### Phase 9: loom/loomtest

**Goal**: the test toolkit subpackage.

Files:
- `pkg/loom/loomtest/memory_sink.go`
- `pkg/loom/loomtest/mock_chatmodel.go`
- `pkg/loom/loomtest/assert.go`

Verification: write an agent unit test with loomtest and run its assertions.

### Phase 10: wolowork integration

**Goal**: migrate the react / pro_report executors onto loom.

- write the wolowork EntSink (implementing Sink + Repository + CloseDetector)
- the react executor → a loom handler + StreamLLMToStep
- the pro_report executor → a loom handler + nested Step

This step is **not inside pkg/loom**, but it is the milestone that validates the loom design.

---

## 11. Risks and open questions

### 11.1 Confirmed risks

- **Renaming the path system**: if decision #5 (one generic struct) is later refactored into a sealed interface, the data shape does not change, but the Writer interfaces may have to be rewritten. **Not happening for now.**
- **Sink performance**: high-frequency deltas (one frame per token) can become a bottleneck with several Tee'd sinks. BufferedSink is the stopgap.
- **A future need for cross-Turn references**: if a sub-agent ever genuinely needs to reference a tool result across the main turn, this has to be redesigned. **Not happening for now; product code wraps it as a Tool.**

### 11.2 Open questions (shelved until needed)

- The shape of a multimodal UserMessage: the first version is Text only.
- Streaming ToolCall increments: the first version does not build in StreamToolCall; product code accumulates ToolCallDelta and writes it once with WriteToolCall.
- Repository transactions/consistency: are cross-Turn writes atomic? The first version writes independently per Run, with no cross-Turn transaction.

---

## 12. The final package layout

```
pkg/loom/
  doc.go                    package introduction
  DESIGN.md                 this document

  # Phase 0: LLM abstraction ✅
  llm.go                    ChatModel / Message / ChatRequest/Response / Chunk / Stream
  call_model.go             CallModel / per-call failover / sync chat tracing
  structured_output.go      ChatStructured / automatic JSON Schema / output retry
  tool.go                   Tool / ToolInfo / ToolCall / ToolCallDelta / ToolRegistry
  errors.go                 provider-neutral sentinel errors

  providers/
    deepseek/               ✅
      provider.go

  # Phases 1-3: data + interfaces
  turn.go                   Turn / TurnStatus / CloseReason
  item.go                   Item / ItemKind / ItemStatus / NoteKind / ItemError
  values.go                 UserMessage / ToolCall (for writing) / ToolResult / ToolError
  writer.go                 the Writer / Step / TurnWriter interfaces
  stream.go                 the ReasoningStream / NoteStream / FinalAnswerStream interfaces
  sink.go                   the Sink / Repository / CloseDetector interfaces
  events.go                 ItemStartedEvent / ItemDeltaEvent / ItemFinishedEvent / LLMCalledEvent
  path.go                   path assembly (optional)

  # Phase 4: the Writer core
  internal/writerimpl/
    writer.go
    step.go
    stream.go

  # Phase 5: Sink implementations
  sinks/
    memory/
    log/
    tee/

  # Phase 6: Run
  run.go                    Run / RunOptions / Handler / the error state machine

  # Phase 7: Otel
  (embedded in internal/writerimpl)

  # Phase 8: helpers
  messages.go               HistoryToMessages
  tools_run.go              Writer.RunTool
  stream_llm.go             StreamLLMToStep

  # Phase 9: tests
  loomtest/
    memory_sink.go
    mock_chatmodel.go
    assert.go
```
