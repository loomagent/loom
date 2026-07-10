# Loom — Agent 框架设计文档

> 本文档记录 loom 的核心设计决策与实施路线。Loom 是 agent 框架,目标是把
> 异构 agent 实现(手写编排 / 任意 LLM SDK)的输出编织成统一事件流,
> fan-out 给多个下游(数据库 / WebSocket / log / otel)。

---

## 1. 定位与目标

### 1.1 Loom 是什么

- **一个 agent 框架的核心抽象层**:wolowork 是第一个使用者,后续可抽独立 module。
- **业务方写 agent 的方式**:实现一个跟 connectrpc handler 同形态的函数(`func(ctx, w TurnWriter, history, input) error`),内部用 Writer 输出事件流。
- **持久化 / 输出推送 / log / otel** 全是可插拔 Sink,业务侧自由组合。
- **轻量定位**:不引入 Graph 编排,纯手写代码 + 统一事件输出 — 跟同类 LLM 编排框架定位相似但更轻。

### 1.2 Loom 不是什么

- **不是 Graph 编排器**:没有 ToolsNode / ChatModelNode 这种节点抽象。
- **不规定业务概念**:Conversation / Chat Mode / 用户系统等业务侧概念走 Metadata 透传,loom 不解释。
- **不是 LLM SDK**:`loom.ChatModel` 是统一 LLM 抽象,但具体 provider(DeepSeek/OpenAI/...)在 `providers/` 子包,后续可独立成 lib。
- **不强制使用某种 agent 模式**:ReAct / 代码编排 / 多 agent 都在同一套 API 下表达。

---

## 2. 核心概念

### 2.1 Turn — 一次 agent 执行

- **执行边界**:从用户提问到 final answer(或失败/取消)。
- **完整数据**:含用户提问 + agent 全部过程(reasoning / tool_call / tool_result / step / final_answer)。
- **可序列化**:用于 UI 完整渲染 + 下一轮对话上下文。
- **两种角度**:
  - `loom.Run(ctx, handler, opts)` → 返回执行后的 `*Turn` 数据快照。
  - Handler 内拿到 `TurnWriter` 句柄,用于写出。

### 2.2 Item — Turn 内任意节点

- 所有内容(user_message / reasoning / step / note / tool_call / tool_result / final_answer)都是 Item。
- **嵌套树形**:Item 之间通过 `Children []Item` 嵌套,不用 ID 引用。
- **Path 自描述**:每个 Item 有 path,如 `turn[0].step[0].step[1].reasoning[2]`。

### 2.3 Step — 嵌套容器

- 表达**代码编排的 sub flow**:阶段 / 轮次 / 子任务 / 任意作用域。
- **任意深度嵌套**:`step → step → step → ...`。
- 业务方主动开:`w.Step(ctx, "label", func(s Step) error { ... })`。
- 闭包结束自动 Close。

### 2.4 Writer — 写出接口

- **业务方调用的高层 API**:OOP 风格,通过 Step / TurnWriter 对象写。
- **不暴露 ItemRef**:嵌套通过 Step 对象传递,业务方零感知。
- **闭包风格**:Step / Stream Writer 都用闭包,框架自动管 Close/Finish/Abort。

### 2.5 Sink — 事件下游

- **可插拔接口**:Ent / WebSocket / Log / Otel / 任何业务实现。
- **不区分"持久化"和"输出"**:都是事件下游,实现自由解释。
- **多个 Sink 通过 TeeSink 组合**:fan-out 给多个目的地。

### 2.6 Repository — 历史召回(读)

- 跨 Turn 的历史加载:`LoadHistory(convID) []Turn`。
- 跟 Sink 分离:Sink 写,Repository 读。
- 业务方实现(从 DB / 内存 / 其它)。

### 2.7 CloseDetector — 外部终结探测(可选)

- 探测 Turn 被外部 cancel/fail(由 dispatcher / StopGeneration 触发)。
- 仅有"权威状态源"的实现需要(如 EntDetector 查 DB);Log/Memory 无需。

---

## 3. 数据模型

### 3.1 Path 体系(全 0-based)

```
turn[0]                                      ← 第 0 个 Turn(turn 在 conversation 内的 index)
turn[0].user_message[0]                      ← 用户提问(单例)
turn[0].step[0]                              ← 第 0 个 step
turn[0].step[0].reasoning[0]                 ← step 内第 0 个 reasoning
turn[0].step[0].tool_call[0]
turn[0].step[0].tool_result[0]
turn[0].step[0].step[0]                      ← sub flow 嵌套
turn[0].step[0].step[0].note[0]
turn[0].final_answer[0]                      ← 最终回答(单例,但带 [0] 保持形态统一)

turn[1].user_message[0]                      ← 同 conversation 下一轮
```

- **0-based**:跟 Go 数组 / OpenAI Responses 一致。
- **每个 parent 下每种 Kind 独立计数**:`step[0].reasoning[0]` 跟 `step[0].tool_call[0]` 并存。
- **Conversation 不在 path 里**:由 Metadata 携带。

### 3.2 Item 结构(通用 struct + Kind 字段)

```go
type Item struct {
    Kind   ItemKind
    Index  uint64       // 同 parent 下同 Kind 的 0-based 下标
    Path   string       // "turn[0].step[1].reasoning[0]"(派生)
    Status ItemStatus

    // Kind-specific 字段
    Text       string      // user_message / reasoning / final_answer / note
    Label      string      // step
    NoteKind   NoteKind    // note
    ToolName   string      // tool_call / tool_result
    ToolCallID string      // tool_call / tool_result
    Arguments  string      // tool_call
    Output     string      // tool_result
    Error      *ItemError  // failed item

    // 嵌套(仅 Kind=step 时非 nil)
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
    NoteSummary    NoteKind = "summary"    // 阶段产物摘要
    NoteDiagnostic NoteKind = "diagnostic" // 异常/诊断信息
    NoteDraft      NoteKind = "draft"      // 流式生成草稿
    NoteRevision   NoteKind = "revision"   // 修订/审阅
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

### 3.3 Turn 数据快照

```go
type Turn struct {
    Index uint64
    Path  string // "turn[0]"

    Items []Item // 完整 item 列表(嵌套树)

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
    Code    CloseCode // 细分原因(封闭枚举)
    Message string
    Cause   error
}

type CloseCode string

// 内置 Code 常量
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

### 3.5 Usage(沿用 LLM 抽象层定义)

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

## 4. Writer 接口体系

### 4.1 三层接口

```go
// Writer:通用写入接口(Turn 根 / Step 都实现)
type Writer interface {
    Path() string

    // 一次性写入(立即 Completed)
    WriteReasoning(ctx context.Context, displayName, text string) error
    WriteNote(ctx context.Context, displayName, text string, kind NoteKind) error
    WriteToolCall(ctx context.Context, displayName string, call ToolCall) error
    WriteToolResult(ctx context.Context, displayName string, result ToolResult) error

    // 流式写入(闭包,框架自动 Finish/Abort)
    StreamReasoning(ctx context.Context, displayName string, fn func(ReasoningStream) error) error
    StreamNote(ctx context.Context, displayName string, kind NoteKind, fn func(NoteStream) error) error

    // sub flow 嵌套(闭包,框架自动 Close)
    Step(ctx context.Context, label string, fn func(Step) error) error
}

// Step:Writer + Step 自身属性(没有显式 Close — 闭包结束自动)
type Step interface {
    Writer
}

// TurnWriter:Writer + Turn 根专属(FinalAnswer 只能在根写)
type TurnWriter interface {
    Writer
    FinalAnswer(ctx context.Context, text string) error
    StreamFinalAnswer(ctx context.Context, fn func(FinalAnswerStream) error) error
}
```

### 4.2 流式 Stream 接口

```go
type ReasoningStream interface {
    AppendText(ctx context.Context, chunk string) error
    SetFinalText(text string) // 可选,覆盖累积值
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

### 4.3 强类型参数

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

### 5.1 Handler 签名

```go
type Handler func(
    ctx context.Context,
    w TurnWriter,
    history []Turn,
    input UserMessage,
) error

type UserMessage struct {
    Text string
    // 预留多模态扩展位置(Images / Files 等)
}
```

### 5.2 Run 入口

```go
func Run(ctx context.Context, h Handler, opts RunOptions) (*Turn, error)

type RunOptions struct {
    Sinks      []Sink
    Repository Repository      // 可选,历史召回
    Detector   CloseDetector   // 可选,外部终结探测
    Tracer     trace.Tracer    // 可选,nil = otel.Tracer("loom")

    History []Turn
    Input   UserMessage

    TurnIndex uint64 // 0 = 自动 len(History)
    Metadata  map[string]string

    OnSinkErr func(Sink, error) // Sink 失败回调;默认 log warn 后继续
    StrictSink bool             // true = Sink 任一失败立即让 Turn 失败
}
```

### 5.3 状态机决定逻辑

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

### 5.4 内置 sentinel error

```go
var (
    ErrContentFilter   = errors.New("loom: content filter")
    ErrLength          = errors.New("loom: length limit")
    ErrTurnClosed      = errors.New("loom: turn closed")
    ErrStepIncomplete  = errors.New("loom: step incomplete")
)
```

业务方:

```go
return loom.ErrStepIncomplete       // step 标 Incomplete,不算错(不冒泡)
return errors.New("real error")     // step 标 Failed,错误冒泡
return nil                          // step 标 Succeeded
```

### 5.5 闭包失败语义

| 闭包返回 | Step / Stream 行为 |
|---|---|
| `nil` | Step Close(Succeeded) / Stream Finish(累积值或 SetFinalText) |
| `ErrStepIncomplete`(sentinel) | Step Close(Incomplete),**err 不冒泡**(被 step 吸收) |
| 其它非 nil error | Step Close(Failed) / Stream Abort(err) + 错误冒泡 |

---

## 6. Sink 体系

### 6.1 Sink 接口

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
    Item      Item // 初始 status=in_progress 的完整 Item
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
    Item      Item // 最终态 Item(含 Status / 完整 payload)
    Time      time.Time
}

type LLMCalledEvent struct {
    TurnIndex uint64
    TurnPath  string
    StepPath  string // 挂在哪个 step 下,空 = turn 根
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

### 6.2 Repository 接口

```go
type Repository interface {
    LoadHistory(ctx context.Context, conversationID string) ([]Turn, error)
    LoadTurn(ctx context.Context, conversationID string, index uint64) (*Turn, error)
}
```

### 6.3 CloseDetector 接口(可选)

```go
type CloseDetector interface {
    CheckClose(ctx context.Context) (CloseReason, error)
}
```

### 6.4 框架自带 Sink

```go
// 测试用:在内存收集所有事件,提供断言 helper
loom.NewMemorySink() *MemorySink

// 调试用:zap 结构化打 log
loom.NewLogSink(logger *zap.Logger) Sink

// 多写组合:并发 fan-out 给多个 sink
loom.TeeSink(sinks ...Sink) Sink

// 过滤:按 predicate 路由
loom.FilterSink(predicate func(any) bool, inner Sink) Sink

// 缓冲(高频 delta 减压):
loom.BufferedSink(inner Sink, batchSize int, flushInterval time.Duration) Sink
```

### 6.5 Sink 错误处理

- 默认 swallow:失败时调 `RunOptions.OnSinkErr(sink, err)`,主流程继续。
- StrictSink=true:任一 Sink 失败立即 markFailed(`Failed`)。
- `AppendText` 流式 chunk 失败始终 swallow:中间丢 delta 不影响最终累积值落库。

---

## 7. 高价值 Helper(随 v1 一起做)

### 7.1 历史 → LLM Messages

```go
func HistoryToMessages(history []Turn, input UserMessage) []Message
```

把嵌套 Turn 列表 + 本轮 input 转成 `[]loom.Message` 喂 LLM:
- `user_message` → `Role=user`
- `final_answer` → `Role=assistant`
- `tool_call` + `tool_result` → 配对成 `Role=assistant`(含 ToolCalls)+ `Role=tool`(含 ToolCallID)

### 7.2 RunTool 合并 tool_call+result

```go
// Step / Writer 上的 helper:写 tool_call → 调 tool.Invoke → 写 tool_result
func (w Writer) RunTool(ctx context.Context, registry *ToolRegistry, name, argsJSON string) (string, error)
```

### 7.3 StreamLLMToStep

```go
// 把 LLM 流自动桥接到 Step 写出:
// - reasoning_content → StreamReasoning
// - content → StreamNote(或自定义 kind)
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

替代现在 react executor 的 StreamConverter,把 ReAct executor 缩到 30 行。

### 7.4 ChatStructured

```go
func ChatStructured[T any](ctx context.Context, purpose string, model ChatModel, req ChatRequest, opts ...StructuredChatOption[T]) (T, *ChatResponse, error)
```

从 Go struct 自动生成 JSON Schema,按 `model.Capabilities()` 选择 provider 原生 `json_schema` / `json_object` / prompt-only,并始终在本地执行 parse + schema validate。
输出不满足结构时会把失败原因写回下一次输入并 retry;业务方可通过 `WithStructuredValidator` 增加领域校验。

### 7.5 CallModel / Failover

```go
func CallModel(ctx context.Context, purpose string, model ChatModel, req ChatRequest, opts ...CallModelOption) (*ChatResponse, error)
```

同步模型调用统一入口。Provider 内部仍负责 transport retry;`CallModel` 负责 tracing 和 per-call failover。
Failover 参考 Eino 形态,由 `ShouldFailover(ctx, attempt)` 决定是否切换,由 `GetFailoverModel(ctx, attempt)` 返回备用模型。`FailoverAttempt.Request` 暴露本次实际请求供判定使用;请求改写不放在 failover policy 中。

---

## 8. 测试工具 `loom/loomtest`

```go
package loomtest

// MemorySink 收集所有事件,提供断言 helper
type MemorySink struct { ... }

func NewMemorySink() *MemorySink
func (s *MemorySink) Events() []Event
func (s *MemorySink) Items() []Item

// MockChatModel 预设响应序列
type MockChatModel struct { ... }

func NewMockChatModel(responses ...MockResponse) *MockChatModel

// 断言
func AssertEvents(t *testing.T, sink *MemorySink, matchers []EventMatcher)
func AssertTurnStatus(t *testing.T, turn *Turn, status TurnStatus)
```

---

## 9. 决策清单(完整 22 项)

| # | 决策 | 已落实 |
|---|---|---|
| 1 | 不支持跨 Turn 引用 |  |
| 2 | 全 0-based,改 DB schema |  |
| 3 | UserMessage 单例,作 Item 带 `[0]`,状态机限制 |  |
| 4 | FinalAnswer 单例,作 Item 带 `[0]`,状态机限制 |  |
| 5 | Item 通用 struct + Kind 字段;Writer 参数用强类型 struct |  |
| 6 | Writer API 用 OOP handle 对象,agent 代码零 ItemRef |  |
| 7 | Timeout 只走 ctx |  |
| 8 | CloseReason 用 `{Code, Message, Cause}`;Status 表达 completed/cancelled/failed 大类 |  |
| 9 | Sub flow = Step 嵌套(代码编排底座);LLM-driven 多 agent 由业务方包装成 Tool |  |
| 10 | Handler 签名 `(ctx, w TurnWriter, history, input)`,Writer 显式传参 |  |
| 11 | Writer / Step / TurnWriter 三层接口 |  |
| 12 | UserMessage 由 Run 自动写入,Writer 不暴露 WriteUserMessage |  |
| 13 | Step / Stream\* 全部闭包风格,框架自动管 Close/Finish/Abort |  |
| 14 | 一次性写入保持命令式 |  |
| 15 | 流式 stream 接口提供 SetFinalText |  |
| 16 | Write\* 改返 error 不返 \*Item |  |
| 17 | Item Status 加 Failed 态;闭包返 ErrStepIncomplete sentinel 不冒泡 |  |
| 18 | Sink 失败默认 swallow + OnSinkErr 回调;strict 模式可配 |  |
| 19 | HistoryToMessages / RunTool / StreamLLMToStep 三个 helper 列入 v1 |  |
| 20 | loom/loomtest 子包(MemorySink / MockChatModel / 断言) |  |
| 21 | TeeSink / LogSink 内置 |  |
| 22 | UserMessage 保持 struct(Text 单字段),为多模态预留 |  |

---

## 10. 实施路线图

按依赖顺序分阶段,每完成一阶段 build/vet 通过 + 简单单测验证。

### 阶段 0 ✅(已完成)

- `pkg/loom/llm.go` — LLM 抽象(ChatModel/Message/Stream)
- `pkg/loom/tool.go` — Tool 抽象
- `pkg/loom/providers/deepseek/` — DeepSeek provider

### 阶段 1:核心数据类型

**目标**:定义 Turn / Item / Path / Status / CloseReason 等无依赖的数据 struct。

文件:
- `pkg/loom/turn.go` — Turn / TurnStatus / CloseReason
- `pkg/loom/item.go` — Item / ItemKind / ItemStatus / NoteKind / ItemError
- `pkg/loom/path.go` — Path 拼装 helper(可选,简单情况内联即可)

验证:纯数据结构,build 通过即可。

### 阶段 2:Sink 接口 + 事件类型

**目标**:定义 Sink / Repository / CloseDetector 接口 + 事件 struct。

文件:
- `pkg/loom/sink.go` — Sink / Repository / CloseDetector 接口
- `pkg/loom/events.go` — ItemStartedEvent / ItemDeltaEvent / ItemFinishedEvent / LLMCalledEvent / DeltaChannel

验证:接口定义,build 通过。

### 阶段 3:Writer 接口

**目标**:定义 Writer / Step / TurnWriter / Stream* 接口。

文件:
- `pkg/loom/writer.go` — Writer / Step / TurnWriter 接口
- `pkg/loom/stream.go` — ReasoningStream / NoteStream / FinalAnswerStream 接口
- `pkg/loom/values.go` — UserMessage / ToolCall / ToolResult / ToolError 强类型 struct

验证:接口定义,build 通过。

### 阶段 4:Writer 内核实现

**目标**:实现 Writer / Step / TurnWriter / Stream* 接口,**含闭包管理**。

关键内部:
- ID/Path 生成器(per-Turn,按 parent + kind 0-based 计数器)
- 状态机(InProgress → Completed/Incomplete/Failed)
- 并发检测(同 step 内 Writer 不可并发使用,panic)
- Sealed 状态(WriteFinalAnswer 后拒绝 Write*)

文件:
- `pkg/loom/internal/writerimpl/writer.go` — 内核
- `pkg/loom/internal/writerimpl/step.go`
- `pkg/loom/internal/writerimpl/stream.go`

验证:单元测试覆盖闭包 Close/Finish/Abort 各种路径。

### 阶段 5:Sink 默认实现

**目标**:MemorySink / LogSink / TeeSink 内置 sink。

文件:
- `pkg/loom/sinks/memory/sink.go` — MemorySink + Events()/Items() 查询
- `pkg/loom/sinks/log/sink.go` — LogSink(zap)
- `pkg/loom/sinks/tee/sink.go` — TeeSink

验证:每个 sink 单元测试。

### 阶段 6:Run + Handler + 错误状态机

**目标**:`loom.Run` 入口、Handler 签名、错误处理、CloseReason 派生。

文件:
- `pkg/loom/run.go` — Run / RunOptions / Handler / 错误派生逻辑
- `pkg/loom/errors.go` — sentinel(ErrContentFilter / ErrLength / ErrStepIncomplete / ErrTurnClosed)

验证:
- 正常完成(WriteFinalAnswer)→ Completed
- return error → Failed(各种 code)
- ctx cancel/timeout → Cancelled
- 编程错误 panic → 直接冒泡,不由 loom.Run recover
- 没 WriteFinalAnswer → Failed("no_final_answer")

### 阶段 7:Otel 内置

**目标**:Writer 内核自动给每个 Item / Step / LLMCall 开 span,挂 ctx baggage。

实现要点:
- ItemStarted 时 `tracer.Start()` 一个 span,挂 `spans[ItemPath]`
- ItemFinished 时 `End()`
- ParentRef 自动嵌套 trace tree
- LLMCalled 用 `gen_ai.*` 标准 attribute

文件:
- `pkg/loom/otel.go`(挂内核里)

验证:跑一个 agent,在 OTLP 兼容后端看到完整 trace tree。

### 阶段 8:Helper

**目标**:HistoryToMessages / RunTool / StreamLLMToStep 三个高价值 helper。

文件:
- `pkg/loom/messages.go` — HistoryToMessages
- `pkg/loom/tools_run.go` — Writer.RunTool / ToolRegistry helper
- `pkg/loom/stream_llm.go` — StreamLLMToStep

验证:用 wolowork react executor 现有代码改造为 helper 驱动版本,行数对比。

### 阶段 9:loom/loomtest

**目标**:测试工具子包。

文件:
- `pkg/loom/loomtest/memory_sink.go`
- `pkg/loom/loomtest/mock_chatmodel.go`
- `pkg/loom/loomtest/assert.go`

验证:用 loomtest 写一个 agent 的单元测试,跑通断言。

### 阶段 10:wolowork 集成

**目标**:把 react / pro_report executor 迁移到 loom。

- 写 wolowork EntSink(实现 Sink + Repository + CloseDetector)
- react executor → loom handler + StreamLLMToStep
- pro_report executor → loom handler + Step 嵌套

这一步**不在 pkg/loom 内**,但是验证 loom 设计的关键里程碑。

---

## 11. 风险与未决问题

### 11.1 已确认风险

- **Path 重命名**:决策 #5(通用 struct)如果将来想用 sealed interface 重构,数据形态不变,但 Writer 接口可能要重写。**暂不发生**。
- **Sink 性能**:高频 delta(每 token 一帧)在 Tee 多 sink 场景下可能成为瓶颈。先做 BufferedSink 兜底。
- **跨 Turn 引用未来需求**:若真有 sub agent 跨主 turn 引用工具结果的需求,需重新设计。**暂不发生,业务方走 Tool 包装**。

### 11.2 未决问题(暂搁置,有需要再决)

- 多模态 UserMessage 形态:第一版只 Text。
- ToolCall 流式增量:第一版不内置 StreamToolCall,业务方累积 ToolCallDelta 后用 WriteToolCall 一次性写。
- Repository 的事务/一致性:跨 Turn 写入是否原子?第一版每次 Run 独立写,不跨 Turn 事务。

---

## 12. 包结构最终形态

```
pkg/loom/
  doc.go                    包介绍
  DESIGN.md                 本文档

  # 阶段 0:LLM 抽象 ✅
  llm.go                    ChatModel / Message / ChatRequest/Response / Chunk / Stream
  call_model.go             CallModel / per-call failover / sync chat tracing
  structured_output.go      ChatStructured / auto JSON Schema / output retry
  tool.go                   Tool / ToolInfo / ToolCall / ToolCallDelta / ToolRegistry
  errors.go                 ErrUnsupported + sentinel(将来加)

  providers/
    deepseek/               ✅
      provider.go

  # 阶段 1-3:数据 + 接口
  turn.go                   Turn / TurnStatus / CloseReason
  item.go                   Item / ItemKind / ItemStatus / NoteKind / ItemError
  values.go                 UserMessage / ToolCall(写入用)/ ToolResult / ToolError
  writer.go                 Writer / Step / TurnWriter 接口
  stream.go                 ReasoningStream / NoteStream / FinalAnswerStream 接口
  sink.go                   Sink / Repository / CloseDetector 接口
  events.go                 ItemStartedEvent / ItemDeltaEvent / ItemFinishedEvent / LLMCalledEvent
  path.go                   Path 拼装(可选)

  # 阶段 4:Writer 内核
  internal/writerimpl/
    writer.go
    step.go
    stream.go

  # 阶段 5:Sink 实现
  sinks/
    memory/
    log/
    tee/

  # 阶段 6:Run
  run.go                    Run / RunOptions / Handler / 错误状态机

  # 阶段 7:Otel
  (内嵌在 internal/writerimpl)

  # 阶段 8:Helper
  messages.go               HistoryToMessages
  tools_run.go              Writer.RunTool
  stream_llm.go             StreamLLMToStep

  # 阶段 9:测试
  loomtest/
    memory_sink.go
    mock_chatmodel.go
    assert.go
```
