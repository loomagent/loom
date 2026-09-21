**简体中文** | [English](DESIGN.en.md)

# Loom — Agent 框架设计文档

> 本文档记录 loom 的定位、核心概念与当前结构。它描述的是**已经实现的**设计,
> 不含尚未实现或已放弃的方案。概念与 API 的名称以代码为准。

---

## 1. 定位与目标

### 1.1 Loom 是什么

- **一个 agent 框架的核心抽象层**:wolowork 是第一个使用者,后续可抽独立 module。
- **业务方写 agent 的方式**:实现一个跟 connectrpc handler 同形态的函数
  (`func(ctx, w TurnWriter, history, input) error`),内部用 Writer 输出事件流。
- **持久化 / 输出推送 / log / otel** 全是可插拔 Sink,业务侧自由组合。
- **轻量定位**:不引入 Graph 编排,纯手写代码 + 统一事件输出 — 跟同类 LLM 编排框架
  定位相似但更轻。

### 1.2 Loom 不是什么

- **不是 Graph 编排器**:没有 ToolsNode / ChatModelNode 这种节点抽象。
- **不规定业务概念**:Conversation / Chat Mode / 用户系统等业务侧概念走 Metadata
  透传,loom 不解释。
- **不是 LLM SDK**:`loom.ChatModel` 是统一 LLM 抽象,但具体 provider 在
  `providers/` 子包。
- **不强制使用某种 agent 模式**:ReAct / 代码编排 / 多 agent 都在同一套 API 下表达。

---

## 2. 核心概念

### 2.1 Turn — 一次 agent 执行

- **执行边界**:从用户提问到 final answer(或失败 / 取消)。
- **完整数据**:含 agent 全部过程(reasoning / tool_call / tool_result / step /
  final_answer)。
- **可序列化**:用于 UI 完整渲染 + 下一轮对话上下文。
- **两种角度**:
  - `loom.Run(ctx, handler, opts)` → 返回执行后的 `*Turn` 数据快照。
  - Handler 内拿到 `TurnWriter` 句柄,用于写出。

### 2.2 Item — Turn 内任意节点

- 所有内容(user_message / reasoning / step / tool_call / tool_result /
  final_answer)都是 Item。
- **嵌套树形**:Item 之间通过 `Children []Item` 嵌套,不用 ID 引用。
- **Path 自描述**:每个 Item 有 path,如 `turn[0].step[0].step[1].reasoning[2]`。

### 2.3 Step — 嵌套容器

- 表达**代码编排的 sub flow**:阶段 / 轮次 / 子任务 / 任意作用域。
- **任意深度嵌套**:`step → step → step → ...`。
- 业务方主动开:`w.Step(ctx, "label", func(ctx context.Context, s Step) error { ... })`。
- 闭包结束自动 Close。

### 2.4 Writer — 写出接口

- **业务方调用的高层 API**:OOP 风格,通过 Step / TurnWriter 对象写。
- **不暴露 ItemRef**:嵌套通过 Step 对象传递,业务方零感知。
- **闭包风格**:Step / Stream Writer 都用闭包,框架自动管 Close/Finish/Abort。

### 2.5 Sink — 事件下游

- **可插拔接口**:Ent / WebSocket / Log / Otel / 任何业务实现。
- **不区分"持久化"和"输出"**:都是事件下游,实现自由解释。
- **多个 Sink 直接传 `[]Sink`**:框架逐个 fan-out,业务侧无需自己写组合器。

### 2.6 历史与上下文

- 历史 Turn 由调用方加载后经 `RunOptions.History` 传入;loom 不关心历史来自
  DB / 缓存 / 内存。
- 拼上下文的转换在 `HistoryToMessages`(见 §7.1),框架不隐式读写历史。

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
turn[0].final_answer[0]                      ← 最终回答(单例,但带 [0] 保持形态统一)

turn[1].user_message[0]                      ← 同 conversation 下一轮
```

- **0-based**:跟 Go 数组 / OpenAI Responses 一致。
- **每个 parent 下每种 Kind 独立计数**:`step[0].reasoning[0]` 跟
  `step[0].tool_call[0]` 并存。
- **Conversation 不在 path 里**:由 `Turn.ConversationID` 携带。

### 3.2 Item 结构(通用 struct + Kind 字段)

```go
type Item struct {
    Kind   ItemKind
    Index  uint64       // 同 parent 下同 Kind 的 0-based 下标
    Path   string       // "turn[0].step[1].reasoning[0]"(派生)
    Status ItemStatus

    // Kind-specific 字段
    Text       string      // user_message / reasoning / final_answer
    Label      string      // step
    ToolName   string      // tool_call / tool_result
    ToolCallID string      // tool_call / tool_result
    Arguments  string      // tool_call
    Output     string      // tool_result
    Error      *ItemError  // failed item

    // 消息来源(仅持久化的 user_message 有意义)
    MessageSource  MessageSource
    MessagePurpose MessagePurpose

    // 嵌套(仅 Kind=step 时非 nil)
    Children []Item

    // Token 累计(仅 Kind=step 有意义)
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
    Code    string // open enum,业务可扩
    Message string
}
```

### 3.3 Turn 数据快照

```go
type Turn struct {
    Index          uint64
    Path           string // "turn[0]"
    ConversationID string
    Items          []Item // 完整 item 树(嵌套结构)

    Status      TurnStatus
    CloseReason *CloseReason // nil = 仍在运行
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

`queued` 由外部(如 dispatcher 预创建 Turn 行)使用;`loom.Run` 直接从
`in_progress` 起步,跑完落入终态。

### 3.5 Usage

```go
type Usage struct {
    PromptTokens         uint64
    CompletionTokens     uint64
    CachedTokens         uint64
    ReasoningTokens      uint64
    ReasoningTokensKnown bool // 区分"显式为零"与"provider 没上报"
    TotalTokens          uint64
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
    WriteReasoning(ctx context.Context, label, text string) error
    WriteToolCall(ctx context.Context, label string, call ToolCall) error
    WriteToolResult(ctx context.Context, label string, result ToolResult) error

    // 流式写入(闭包,框架自动 Finish/Abort)
    StreamReasoning(ctx context.Context, label string, fn func(ReasoningStream) error) error

    // sub flow 嵌套(闭包,框架自动 Close)
    Step(ctx context.Context, label string, fn func(ctx context.Context, s Step) error) error
}

// Step:Writer + 是嵌套容器(没有显式 Close — 闭包结束自动)
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

两个接口方法集相同,由同一个实现同时满足;分开命名只是为了在调用处表达语义
(推理过程 vs 最终回答)。

```go
type ReasoningStream interface {
    AppendText(ctx context.Context, chunk string) error
    SetFinalText(text string) // 可选,覆盖累积值
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
    ID        string
    Arguments string
}

type ToolResult struct {
    CallID   string
    ToolName string
    Output   string
    Err      *ItemError // 非 nil 时 result item 标 Failed
}
```

---

## 5. Handler API 与 Run

### 5.1 Handler 签名

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

### 5.2 Run 入口

```go
func Run(ctx context.Context, h Handler, opts RunOptions) (*Turn, error)

type RunOptions struct {
    Sinks      []Sink
    History    []Turn
    Input      UserMessage

    ConversationID string            // 必填,Run 校验非空
    TurnIndex      uint64            // 0 = 自动 len(History)
    Metadata       map[string]string

    OnSinkErr      func(Sink, error) // 默认 nil = 静默 swallow
    StrictSink     bool              // true = 任一 Sink 失败立即让 Turn Failed
    CaptureContent bool              // 是否把 prompt/completion 等写进 span
}
```

Tracer 不在这里:OTel 走 SDK 的全局 TracerProvider,未配置时是 noop tracer。

### 5.3 CloseReason 派生

```
优先级:
  1. closeReason 已设(FinalAnswer 自封口)        → 保留
  2. strict 模式 sinkErr                          → {Failed, agent_error}
  3. ctx.DeadlineExceeded                         → {Cancelled, timeout}
  4. ctx.Canceled + context.Cause                 → {Cancelled, user_cancel |
   |                                                   host_shutdown | external_cancel}
  5. handlerErr != nil:
       - 取消类错误                              → {Cancelled, ...}
       - ErrContentFilter                        → {Failed, content_filter}
       - ErrOutputTruncated                      → {Failed, output_truncated}
       - 其它                                     → {Failed, agent_error}
  6. handlerErr == nil 且没写 FinalAnswer          → {Failed, no_final_answer}
```

### 5.4 内置 sentinel error

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

- `ErrContentFilter` 对应模型已经返回 `finish_reason=content_filter`;
  `ErrSensitiveContentRisk` 对应 provider 在建立请求阶段直接返回错误(常见 HTTP
  400),调用点拿不到 `ChatResponse`。provider 负责把自己官方的错误类型映射到后者,
  上层 policy 不应匹配 provider 私有文案。

### 5.5 闭包失败语义

| 闭包返回 | Step / Stream 行为 |
|---|---|
| `nil` | Step Close(Completed) / Stream Finish(累积值或 SetFinalText) |
| 取消类错误 | Step Close(Cancelled),error 继续向上传递 |
| 其它非 nil error | Step Close(Failed,Error 填),error 向上传递 |

---

## 6. Sink 体系

### 6.1 Sink 接口与事件

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
    Item      Item // 初始态完整 Item
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
    Item      Item // 终态完整 Item(含 Status / 完整 payload)
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
```

### 6.2 框架自带 Sink

```go
// 测试与调试:在内存收集所有事件,并提供按类型查询与 Reset
loom.NewMemorySink() *MemorySink
```

持久化 / 前端推送 / 指标等由业务侧实现 `Sink`。

### 6.3 Sink 错误处理

- 默认 swallow:失败时调 `RunOptions.OnSinkErr(sink, err)`,主流程继续。
- `StrictSink=true`:任一 Sink 失败立即 markFailed。
- `AppendText` 流式 chunk 失败始终 swallow:中间丢 delta 不影响最终累积值落库。

---

## 7. Helper

### 7.1 历史 → LLM Messages

```go
func HistoryToMessages(history []Turn, input UserMessage) ([]Message, error)
func AppendAssistantTurn(msgs []Message, resp *ChatResponse, results []ToolExecResult) []Message
```

`HistoryToMessages` 把嵌套 Turn 列表 + 本轮 input 转成 `[]loom.Message` 喂 LLM:
- `user_message` → `Role=user`
- `reasoning` → 累积到紧随其后第一条 assistant message 的 `ReasoningContent`
- `tool_call` / `tool_result` → 配对成 `Role=assistant`(含 ToolCalls)+ `Role=tool`
- 历史里出现未正常结束的 Turn 会返回 error,而不是拼出可疑上下文

### 7.2 执行工具

```go
func ExecuteToolCalls(ctx context.Context, w Writer, registry *ToolRegistry, calls []ToolCall) ([]ToolExecResult, error)
func RunToolByName(ctx context.Context, w Writer, label string, registry *ToolRegistry, name string, args any) (string, error)
```

- `ExecuteToolCalls` 走 LLM 返回的 ToolCalls,逐条写 tool_call → invoke → 写
  tool_result;单条失败不中断,`ToolExecResult.Err` 记录。
- `RunToolByName` 是代码编排场景:args 是任意 Go 值(内部 marshal),callID 由
  loom 生成(`call_0` / `call_1` / …)。

### 7.3 StreamLLMToStep

```go
func StreamLLMToStep(ctx context.Context, w Writer, purpose string, model ChatModel, req ChatRequest) (*ChatResponse, error)
```

把 LLM 流式输出桥接到 Writer:
- `reasoning_content` chunk → 实时写出 reasoning item(首个 chunk 才开,不落空 item)
- `content` chunk → 累积到返回的 `ChatResponse.Content`
- `tool_call` chunk → 按 Index 拼成完整 ToolCall 返回(不写 tool_call item;
  由 §7.2 在真正 invoke 前写,保证 tool_call/tool_result 配对)

### 7.4 结构化输出

```go
func ChatStructuredArgs(ctx context.Context, purpose string, model ChatModel, req ChatRequest, contract *ArgsContract, opts ...StructuredOption) (Args, *ChatResponse, error)
```

用与工具参数**同一套** `ArgsContract` 约束模型返回值:provider 原生支持
`json_schema` 时传同一份 schema,仅支持 `json_object` 时退化为 JSON object + 提示词
约束,本地始终按契约校验并支持输出重试。

### 7.5 同步调用与 failover

```go
func CallModel(ctx context.Context, purpose string, model ChatModel, req ChatRequest, opts ...CallModelOption) (*ChatResponse, error)
```

同步模型调用的统一入口,负责 tracing 与 per-call failover。Provider 内部仍负责
transport retry。failover 由 `ShouldFailover` / `GetFailoverModel` 决定切不切、切到
哪个模型。

---

## 8. 包结构

```
github.com/loomagent/loom/
  doc.go                        包介绍

  # 声明式参数契约与 schema
  args.go args_arg.go           整调用校验 / 类型化句柄(String/Uint/Enum/Date/…)
  args_contract.go              ArgsContract:构建、编译、Decode
  args_error.go                 ToolArgumentError 与面向模型的错误
  argument_guidance.go          expected / example arguments 摘要
  schema.go schema_model.go     loom.Schema 模型
  schema_error.go               violation → 面向模型的文案
  structured_output.go          ChatStructuredArgs

  # 模型抽象与调用
  llm.go                        ChatModel / Message / ChatRequest/Response / Chunk / Stream
  reasoning_contract.go         显式推理开关与强度解析
  call_model.go                 CallModel / per-call failover
  retry.go                      共享 retry 调度(Transient / RateLimit / Permanent)
  tracing.go                    OTel span
  llmadmission/                 按凭据的有界并发准入
  attempt_admission.go          AttemptMeta(真实供应商配额)
  modelfactory/                 显式构造模型

  # 数据模型与执行
  turn.go item.go values.go     Turn / Item / UserMessage / ToolCall / ToolResult
  run.go                        Run / RunOptions / CloseReason 派生
  scope.go turn_root.go         Writer 内核与 Turn 根
  writer.go stream.go stream_impl.go  Writer 体系与流式实现
  sink.go sinks.go events.go    Sink 体系与 MemorySink
  usage_context.go              usage scope 绑定

  # 工具与 helper
  tool.go                        Tool / ToolInfo / ToolRegistry / ArgsTool
  tools_run.go                   ExecuteToolCalls / RunToolByName
  messages.go                    HistoryToMessages / AppendAssistantTurn
  stream_llm.go                  StreamLLMToStep
  handlerregistry/              显式 handler 注册
  tools/calculator gettime web/…

  # 校验与对照
  internal/schema/               schema 模型本体
  internal/toolcontract/         校验器 + 官方 JSON Schema Test Suite 子集

  # 周边框架
  modelprobe/                    基于真实行为的模型能力探测
  contextpolicy/                 可组合的上下文构建策略
  react/ react/review/           与 provider 无关的 ReAct 运行时与质量闸门
  prompttemplate/                占位符校验与渲染
  sourceregistry/                源去重与稳定引用

  # providers
  providers/ark deepseek openrouter zhipuai serper unifuncs
  providers/internal/openaicompat/  三个 OpenAI wire provider 共享的翻译
  providers/internal/probe/         四个 provider 共享的探针序列化

  scripts/check-coverage.sh      覆盖率门槛
```

---

## 9. 设计决策

| # | 决策 | 状态 |
|---|---|---|
| 1 | 不支持跨 Turn 引用 | 已落实 |
| 2 | 全 0-based | 已落实 |
| 3 | UserMessage 单例,作 Item 带 `[0]` | 已落实 |
| 4 | FinalAnswer 单例,作 Item 带 `[0]`,状态机限制 | 已落实 |
| 5 | Item 通用 struct + Kind 字段;Writer 参数用强类型 struct | 已落实 |
| 6 | Writer API 用 OOP handle 对象,agent 代码零 ItemRef | 已落实 |
| 7 | Timeout 只走 ctx | 已落实 |
| 8 | CloseReason 用 `{Code, Message, Cause}`;Status 表达大类 | 已落实 |
| 9 | Sub flow = Step 嵌套;LLM-driven 多 agent 由业务方包装成 Tool | 已落实 |
| 10 | Handler 签名 `(ctx, w TurnWriter, history, input)` | 已落实 |
| 11 | Writer / Step / TurnWriter 三层接口 | 已落实 |
| 12 | user_message 不由 Writer 写出,由调用方先持久化并传入 | 已落实 |
| 13 | Step / Stream\* 全部闭包风格,框架自动管 Close/Finish/Abort | 已落实 |
| 14 | 一次性写入保持命令式 | 已落实 |
| 15 | 流式接口提供 SetFinalText | 已落实 |
| 16 | `Write*` 返 error 不返 `*Item` | 已落实 |
| 17 | Item Status 含 Failed / Cancelled 态 | 已落实 |
| 18 | Sink 失败默认 swallow + OnSinkErr;strict 模式可配 | 已落实 |
| 19 | HistoryToMessages / 工具执行 / StreamLLMToStep 三个 helper | 已落实(见 §7) |
| 20 | 历史由调用方经 `RunOptions.History` 传入,框架不定义 Repository | 已落实 |
| 21 | 框架只自带 MemorySink,其余 Sink 由业务侧实现 | 已落实 |
| 22 | 工具参数契约与结构化输出共用同一套 `ArgsContract` | 已落实 |
| 23 | 显式推理:每个调用点必须声明开关,启用时必须显式选强度 | 已落实 |
| 24 | schema 关键字集合是封闭的,解码拒绝集合外的关键字 | 已落实 |
| 25 | 工具契约按声明式参数 + 类型化句柄,不用 struct tag | 已落实 |
| 26 | `ValidateSchema` 每次调用重新编译 schema;不缓存,因为 schema 是调用方可能修改的普通值 | 已落实 |

已放弃:Note 系列(reasoning 与 label 足以表达过程信息)、CloseDetector(外部终结
由调用方取消带 cause 的 ctx 表达)、`Writer.RunTool`(由 `RunToolByName` 与
`ExecuteToolCalls` 覆盖)、`StreamToolCall`(流式增量由调用方累积)。

---

## 10. 风险与未决问题

### 10.1 已确认风险

- **Sink 性能**:高频 delta(每 token 一帧)在多个 Sink 场景下可能成为瓶颈。框架不
  内置缓冲 Sink,需要时由业务侧实现。
- **provider 间的重复**:三个 OpenAI wire provider 共享翻译,但 `Recv` / `Chat` /
  `streamAdapter` 仍各自维护;共享它们需要一组 provider 级 hook,收益与复杂度尚未
  证明划算。
- **ark 的 wire 差异**:ark 用火山 SDK,不共享 `openaicompat`;其翻译层必须有自己的
  测试(已补齐)。

### 10.2 未决问题

- **多模态 UserMessage**:当前只有 Text + Source/Purpose。
- **跨 Turn 引用**:若真有 sub agent 跨主 turn 引用工具结果的需求,需重新设计;当前
  由业务方走 Tool 包装。
- **结构化推理的完整回传**:OpenRouter 的 `reasoning_details`(加密 / 摘要型推理)必须按原样、按原顺序回传,而 `loom.Message` 只有一个 `ReasoningContent string`。当前只回传纯文本形式;要完整回传需要给 Message 增加一个 provider 不透明的原始 JSON 载体。
- **规则型约束的可见性**:`NotBlank` 之类整调用/字段级校验不出现在
  expected-arguments 摘要里,模型只有失败后才知道;要让它们可见需要 builder 携带
  面向模型的说明文字。
