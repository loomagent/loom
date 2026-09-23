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
  1. strict 模式记录的 sinkErr                     → {Failed, agent_error}
     (排在封口之前:失败的那次写入可能正是持久化 final answer 的那次)
  2. closeReason 已设(FinalAnswer 自封口)        → 保留
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
`json_schema` 时传同一份 schema,只支持 `json_object` 时发出一条 JSON object 请求,
声明不支持的模型直接报错,本地始终按契约校验。schema 绝不写进提示词:
靠改写对话来约束模型,是一种隐藏请求。loom 只问一次:非法响应以 `*StructuredOutputError`
带着响应内容返回,再试几次、带什么上下文由调用方决定(react 轮次会把错误连同其余对话
一起反馈给模型)。

`json_object` 模式下提示词由调用方持有,契约只交出**构建时已校验过**的材料:`contract.Example()`
给出满足 schema 的样例,`contract.JSONObjectPrompt()` 给出"要 JSON + 样例 + 字段约束"的完整
引导语。框架从不自行把其中任何一段放进请求。

### 7.5 同步调用与 failover

```go
func CallModel(ctx context.Context, purpose string, model ChatModel, req ChatRequest, opts ...CallModelOption) (*ChatResponse, error)
```

同步模型调用的统一入口,负责 tracing 与 per-call failover。Provider 内部仍负责
transport retry。failover 由 `ShouldFailover` / `GetFailoverModel` 决定切不切、切到
哪个模型。

---

### 7.6 Responses API 评估:暂不采用

**结论**:不引入 Responses API 适配器——它独有的能力我们大多已经在 chat completions 上拿到,而它不可移植的那部分不值得绑定一家 provider。

**实测矩阵**(四家;原始 HTTP 与 `openai-go/v3` 两条路径都验过)

| provider | `/responses` | reasoning item | `message.phase` | 工具调用 | `text.format` | 回传历史(推理+调用+结果) |
|---|---|---|---|---|---|---|
| DeepSeek | ✓ `/v1/responses` | ✓ 含 `encrypted_content` | **✓ `commentary` 与 `final_answer` 都实测到** | ✓ | ✓ | ✓ |
| 火山方舟 | ✓ `/api/v3/responses` | ✓ `summary` + encrypted | ✗ | ✓ | ✓ | ✓ |
| 智谱 | ✓ `/api/v1/responses` | ✓ 无 summary / encrypted | ✗ | ✓ | **✗ 返回 Markdown** | **✗ 400(多轮需 `store` + `previous_response_id`)** |
| OpenRouter(grok) | ✓ `/api/v1/responses` | ✓ `summary` + encrypted | ✗ | ✓ | ✓ | ✓ |

`phase` 只有 DeepSeek 提供,且 SDK 注明它是模型族行为("For models like `gpt-5.3-codex` and beyond"),不是平台保证;它还是 **message 级**标记——`final_answer` 的消息里同样可能混着解说。

**不采用的理由**

1. 它唯一真正独有且对 loom 有用的能力是"reasoning 与正文结构分离",而 chat completions 路径**已经在承载同一份数据**:`ReasoningContent` 加 `ReasoningDetails`,后者保存的原始 JSON 就是 `summary` / `encrypted_content`。
2. 第二个独有能力 `phase` **不可移植**(只有一家,且是模型行为),把判断压在它上面等于绑 provider。
3. 它的架构倾向是服务端多轮(`store` + `previous_response_id`),与本设计"历史由调用方持有,框架不定义 Repository"(决策 20)相反。
4. 迁移成本真实:items 化协议、事件流改为等 `response.completed`,以及四家的差异税(智谱的 Responses 残缺)。
5. 结构化输出在其上没有增量,反而多一处差异(智谱 `text.format` 不生效)。

**这个评估不改变的部分**(与 API 选择无关)

工具轮的 `content` 可能与工具调用同现,且其中已经含有结论片段(实测:一个响应里 `reasoning` + `content` + `tool_call`,而 `content` 里是推算结果)。所以**交付正文只能来自"工具已被移除"的轮次**,而这一点 chat completions 完全支撑:终止工具 + 物理移除工具 + `tool_choice`。

**何时重新评估**

1. 主力模型换成 OpenAI 系且只走一家时(那时 `phase` 与 reasoning items 才有稳定收益);
2. 需要接入只讲 Responses 协议的客户端/工具链时(兼容问题,不是能力问题);
3. 某家不再暴露 chat completions、只剩 Responses 时。

### 7.7 终止工具与终局状态

**问题**(实测):工具轮的 `content` 可能与工具调用同现,且其中已含结论片段;而"没有 tool call"这一个信号同时承载"完成"与"未按协议完成"(停顿、计划、拒绝、把调用写成伪标记)。因此**交付正文只能来自"工具已被移除"的轮次**,并且需要把"何时进入该轮次"变成可校验的提交点。

**设计**

1. **数据标记,不是策略**:工具上带 `EndsToolPhase`(上游 API:`WithEndsToolPhase()`)。名字由调用方定,框架不引入阶段枚举。
2. **只有成功才结束**:失败的执行把错误作为工具结果回给模型,阶段不结束。
3. **终局保证**:带标记的工具成功后,工具被物理移除;此后每次请求都不带工具,其 `content` 按构造就是正文;终局不可逆;每个 Turn **至多提交一份**最终答案——失败的流是**候选**,不是已提交答案。
4. **混批整批拒绝**:同一响应里只要含**有效**的终止工具,该批次必须**恰好一个 call**。混批时**整批都不执行**,为批次里每个 call 写一条错误结果(协议要求每个 `call_id` 都有结果,否则下一轮请求不合法),阶段不结束。

   依据(实测 9 组、约 30 次调用;三个模型 × 三个任务变体):

   | 批次构成 | 次数 |
   |---|---|
   | 只普通工具 | 8 |
   | 终止工具单独调用 ✓ | 8/9 的运行出现 |
   | **混批 ✗** | **1**(grok:`[calculator, finalize_answer]`,即"最后一次计算 + 收尾") |
   | 终止工具重复调用 ✗ | 0 |
   | 工具移除后写出正文 | 9/9 |

   实测还表明:模型倾向于把**最后一次普通调用**与终止工具捆在一起;而只拒终止调用会让同批的副作用工具**重复执行**(模型视整批失败而重发),整批预检是唯一能保证"这一批没有发生任何事"的规则。

5. **错误文案必须给正确指引**:"本批所有调用均未执行,工具阶段仍然开放。请先重新提交仍然需要的普通工具调用;确认不再需要工具后,再单独调用 `{终止工具名}`。不要把本批任何调用视为已成功。"(若直接让模型"下一轮单独调用终止工具",等于引导它跳过本批里它本来要补的证据。)
6. **框架不内置恢复策略**:漏调、提前调用、重试/撤回/换模型归调用方;产品曾加过自动恢复并主动收窄删掉,与"框架给原子接口与保证,策略归调用方"一致。

**实现约束**(评审得出的通过条件)

- 预检必须在**任何执行之前**,且先于工具预算扣减、执行钩子与并发调度。
- 规则按**调用数量**定义(含有效终止工具 ⇒ 恰好一个 call);标记只能来自 registry 的工具定义,不能凭模型输出的名字认定。
- **重复混批必须消耗模型轮次/token/时间预算**,不能被计为进展,否则连续混批会绕过空转保护。
- **错误必须真的到达模型**:只设内部错误字段不够,工具结果的内容里要带上错误文本。
- **三个事件分开**:工具阶段结束、正文候选生成、Turn 封口。移除工具只保证"生成发生在无工具阶段",不保证内容可交付;软着陆与正常终止进同一阶段但保留不同的结束原因。
- **"至多提交一份最终答案"由原子提交保证**(原先只靠 `ErrTurnClosed` 是不够的:封口前存在并发提交与"流式失败后重试"两个窗口)。实现:提交在同一临界区内**认领并落地**——一次性路径在同一锁区里完成"检查未封口 → 追加 item → 封口",所以答案与封口同时可见;流式路径在闭包运行期间持有认领,结束时**在锁内**决定"提交封口"或"标失败并释放认领",失败因此仍可重试。
- 因此提交的语义是**候选与提交分离**:失败的流留下一个 `failed` 的审计 item,只有 `completed` 的那份进入 LLM 历史(M6 的历史转换按状态过滤;此前按 `Kind` 读取会把被拒绝的草稿喂给模型)。
- 认领期间的第二个提交返回**独立错误** `ErrFinalAnswerInProgress`,不用 `ErrTurnClosed`——后者被 `IsCancelError` 视作取消,会把"另一个提交在途"误报成取消并干扰终态推导。封口之后仍返回 `ErrTurnClosed`。
- 闭包 **panic** 与失败同等处理:先标记 item 失败并释放认领,再把 panic 抛给调用方(框架此前不 recover panic,这里只保证状态一致、不吞 panic)。
- 边界:终止工具成功与状态提交之间存在故障窗口(重启后是否重跑),终止工具最好无副作用或幂等;"整批未执行"只覆盖框架控制的工具,供应商托管工具不适用。
- 实现中的取舍:带标记的工具**不受工具预算约束**(它只在 `availableTools` 与 `reserveTool` 两处豁免;否则预算耗尽时阶段无法结束);混批拒绝复用一个新错误码 `terminal_tool_not_alone`(与既有的 `tool_budget_exhausted` 并列),错误文本随工具结果一起回到模型;`Config.ToolPhaseEndedPrompt` 可覆盖终局提示语。

**交付归属:归调用方。** 框架管阶段不可逆、执行边界、生成状态、错误与用量,以及原子的最终提交/封口;调用方管全文审校、草稿替换、展示方式,以及提交哪一份。据此把"**产生候选**"与"**提交答案**"分开:终局之后可以多次做**无工具**的草稿生成或审校,最后只提交一份 canonical answer;重写草稿不等于重开工具阶段。若将来框架提供流式交付,必须是**显式选择**(首个增量一旦公开,就不能再承诺全文替换而没有撤回机制)。

**尚需更多真实样本**:"整批拒绝"是否应作为产品默认,以及在以只读工具为主的产品里"只拒终止调用"是否更优,需要更多混批样本比较完成率与额外轮次。

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
  format_validation.go          format 语义检查(日历 / 时钟 / 闰秒)
  structured_output.go          ChatStructuredArgs

  # 模型抽象与调用
  llm.go                        ChatModel / Message / ChatRequest/Response / Chunk / Stream
  reasoning_contract.go         显式推理开关与强度解析
  call_model.go                 CallModel / per-call failover
  retry.go                      共享 retry 调度(Transient / RateLimit / Permanent)
  tracing.go                    OTel span
  attempt_admission.go          per-attempt 准入缝(节流策略由宿主提供)
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
  testdata/format/               官方 optional/format 用例(date/time/date-time/uuid)

  # 周边框架
  modelprobe/                    基于真实行为的模型能力探测
  livetest/                      由 live.env 驱动的真实端点 E2E
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
| 27 | 框架不内置节流策略(并发窗口 / AIMD / 熔断阈值都是部署策略),只提供每次物理尝试的准入缝 | 已落实 |
| 28 | provider 自己的结构化推理以原始 JSON 按序往返;Message / ChatResponse / Chunk 各带一个不透明载体,loom 不解释,流式按官方规则逐帧拼接 | 已落实 |
| 29 | 框架不写提示词、不做输出重试:只问一次,把响应与错误原样交回;重试次数与第二次带什么由调用方决定 | 已落实 |
| 30 | `json_object` 模式下提示词由调用方持有;契约只交出构建时校验过的样例与字段引导语(`Example()` / `JSONObjectPrompt()`) | 已落实 |
| 31 | `integer` 按值判定(精确有理数运算);`format` 的断言在契约层,schema 层保持规范的注解语义 | 已落实 |
| 32 | 子集套件的跳过分三类并各自断言:未建模关键字(策略 18)/ 关键字写法(策略 4)/ 未实现构造(缺口 14) | 已落实 |

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
- **规则型约束的可见性**:`NotBlank` 之类整调用/字段级校验不出现在
  expected-arguments 摘要里,模型只有失败后才知道;要让它们可见需要 builder 携带
  面向模型的说明文字。
