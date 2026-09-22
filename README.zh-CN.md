[English](README.md) | **简体中文**

<!-- Loom 运行时的概览、用法与集成指南。 -->
# Loom

Loom 是一个轻量、事件驱动的 Go agent 运行时。它把一次 agent 执行变成结构化的
事件流,可以交给持久化、实时 UI、日志和可观测性等下游消费。

Loom 刻意保持小:agent 的控制流仍然是普通的 Go 代码,而模型、工具、事件下游和
对话历史都是可替换的接口。

## 特性

- 结构化的 Turn、嵌套 Step、推理、工具调用和最终回答
- 流式的模型 API 和 Writer API
- fan-out 到多个可插拔的 `Sink` 实现
- 与 provider 无关的 `ChatModel` 抽象
- 推理过程的回传:下一轮调用会带回上一轮的推理,文本之外,若 provider 返回结构化推理块,则原样按序带回
- OpenTelemetry 链路追踪,默认不采集内容
- 内置 Ark、DeepSeek、OpenRouter、智谱 AI 的 provider

## 安装

```bash
go get github.com/loomagent/loom
```

Loom 目前要求 Go 1.27 或更高版本。

## 快速开始

两个可直接运行的示例不需要 API key、也不需要联网:

```bash
go run ./examples/quickstart   # 一个 Turn,以及 Sink 从它收到了什么
go run ./examples/react        # ReAct 循环调用一个工具(模型用脚本扮演)
```

下面的代码与第一个示例是同一个形状。

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

## 工具

工具用 `ArgsContract` 和 `NewArgsTool` 声明。整份契约——公开的工具名、每个参数的
类型、描述、是否必填以及各种约束——只写一次,handler 通过类型化句柄读取参数,因此
不涉及 Go struct、不涉及 struct tag,读取处也不出现字符串键。

工具名通常是包级常量。`ValidateToolName` 和 `NewArgsContract` 要求 1–64 个字符且
匹配 `^[a-z][a-z0-9_]{0,63}$`;`ToolRegistry.Register` 施加同样的校验,并拒绝重名。

一份契约一次性绑定 schema、编译好的校验器和错误契约,并且不可变、可并发调用;
只编译一次也避免了每次调用都重建 schema。参数在句柄读取之前一直保持原始 JSON,
所以整数能保住完整的 64 位范围,而不会先经过 float64 被取整。

出错时暴露 `ToolArgumentError` 元数据,并渲染出一段有长度上限、紧凑、非 JSON 的
`expected arguments` 契约供模型自我纠正,而不是把整份 schema 倒给模型。当声明的
示例能组成一次完整调用时,还会附带一个已通过校验的 `example arguments` JSON 对象。

工具契约也覆盖交互的另一个方向——模型作为结构化输出返回的那个对象。
[工具契约](#工具契约) 用同一套类型化句柄同时声明两者。

## 工具契约

一份工具契约就是一次类型化句柄的声明,而同一份声明同时服务模型交互的两个方向:
模型发给工具的参数,以及模型作为结构化输出返回的对象。两者都通过同一套句柄读取,
也都由同一份 schema 约束。

### 参数——模型发给工具的内容

每个参数是一个类型化句柄;句柄既是声明,也是 handler 读取该值的方式:

```go
query := loom.String("query").Required().MinLen(1).Desc("Google search query.")
resultType := loom.Enum("type", "search", "news").Desc("Result type; defaults to search.")
dateFrom := loom.Date("date_from").Desc(`Optional lower bound, e.g. "2026-08-17".`)
dateTo := loom.Date("date_to").Desc(`Optional upper bound, e.g. "2026-08-18".`)
limit := loom.Uint("limit").Max(20).Desc("Maximum results to return.")

// A whole-call rule closes over the handle it points at, so the diagnostic names
// the field without a string literal.
validateDateRange := func(_ context.Context, from, to string) error {
	if from != "" && to != "" && from > to {
		return loom.InvalidOn(dateTo, "date_to must not precede date_from")
	}
	return nil
}

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

`Get` 返回的参数类型在声明处就已确定,所以读取处既没有字符串键也没有类型断言。
模型省略的可选参数读出来是零值,而 `Present` 能区分"未提供"和"提供了但为空"。
整数是无符号的(`Uint`),所以计数类参数不可能为负,schema 同样拒绝负数。
`Date`、`Time`、`DateTime`、`UUID` 同时投影出 `format` 和匹配的形状 `pattern`,
因此忽略 `format` 的 provider 仍然能约束该值。`NotBlank` 用的是标准 schema 写法
("非空且非纯空白"),所以它经 schema 到达模型,而不只是出现在失败文案里。它约束的是
**内容**而非**存在性**:模型省掉一个可选参数仍然通过,要求必填仍用 `Required`。
当一个参数需要多个 `pattern` 时(格式的形状 + 显式 pattern,或格式 + `NotBlank`),
多出来的那些成为 `allOf` 分支,两者同时生效,而不是互相覆盖。未知参数默认被拒绝。

字段级检查接收 `FieldValidator[T]`;整调用级检查用 `Cross` 声明,它接收自己要读的
类型化句柄:

```go
loom.Cross(dateFrom, dateTo).Using(validateDateRange)
```

句柄让规则的依赖成为契约的一部分:构建契约时每个句柄都会对照已声明的参数校验;
当规则涉及的参数一个都没出现时,该规则被跳过;检查函数拿到的是类型化的值而不是
`Args`。请给规则起名:内联字面量也能用,但具名函数可单独测试,也能在堆栈里被认出。

把错误指向某个字段时用句柄,而不是字符串。定义在句柄旁边的规则闭包捕获它们,调用
`InvalidOn(dateTo, ...)`。无法闭包捕获句柄的包级规则改用 `InvalidAt("date_to", ...)`;
这个名字会对照已声明的参数校验,不匹配时按内部错误处理,而不是当作面向模型的纠正
请求。校验器用 `Invalid`、`InvalidAt` 或 `InvalidOn` 上报面向模型的问题;任何其它
错误都视为内部故障,并且 `errors.Join` 可以让一个校验器上报多个问题。

校验分两层。JSON Schema 先跑,负责类型、是否出现、枚举以及声明的范围和格式约束;
它一旦拒绝该调用,已声明的校验器就不跑了,因为这些校验器假定值的形状是对的。
schema 通过之后,所有校验器都会执行,问题会被收集起来,模型因此能在一次交互里拿到
全部业务规则违例,而不是每重试一次才拿到一条。所以校验器必须廉价、无副作用,且在
别的校验器已经失败时仍然安全。

### 响应——模型作为结构化输出返回的对象

`ChatStructuredArgs` 使用与工具参数完全相同的 `ArgsContract`——相同的
`String` / `Uint` / `Enum` 声明、相同的 schema、相同的类型化句柄——只是在这里契约
约束的是模型返回什么,而不是模型发送什么。provider 通过原生 `json_schema` 收到这份
schema;若它只声明支持 `json_object`,则发出一条 `json_object` 请求。Loom 绝不把
schema(或任何调用方没写的东西)写进提示词:声明“不支持结构化输出”的模型会直接报错
而不是照样发出去,能力未声明时则原样发出。在 `json_object` 模式下,提示词得自己提出
JSON **并给出期望的 JSON 样例**:DeepSeek 的
[文档](https://api-docs.deepseek.com/zh-cn/guides/json_mode)要求 system 或 user prompt
必须含有 json 字样、并给出样例,否则回
`400 Prompt must contain the word 'json' in some form to use 'response_format' of type 'json_object'`
——loom 把它当作请求层永久失败,不会重试。那份文档还有两点影响这条路径:`max_tokens`
要留够空间,截断的响应会按“非法输出”上报;API 有概率返回空的 content,上报方式相同。
承载这份形状的是两个方法:`contract.Example()` 返回契约从声明的 `Example` 值组装并校验过的
样例;`contract.JSONObjectPrompt()` 返回完整的那段引导语——

```text
Answer with JSON of exactly this shape:
{"confidence":0.6,"notes":"fast but damaged","verdict":"mixed"}

Fields: confidence=<number, required, 0..1>; notes=<string, required, min length 1, max length 80>; verdict=<string, required, one of ["positive","negative","mixed"]>
```

loom 依然不会自己把它放进请求:**由调用方索取、由调用方决定放哪**——所以用户自己的任务描述
和它拼在一起时,没有任何东西被改写:

```go
messages := []loom.Message{
    {Role: loom.RoleSystem, Content: contract.JSONObjectPrompt()},
    {Role: loom.RoleUser, Content: "判断这条配送评价的整体情绪。"},
}
```

也正因如此,**格式要求只应写在一处**。任务描述该说**判断什么、权衡什么、适用哪些业务规则**;
**形状由契约给**。对 `json_object` 模型的三次真跑说明了原因:引导语单独成一条消息时,第一次
就满足契约;而任务里又要求"先用一句话说明理由"的那次,那句要求被**静默丢弃**——
`response_format: json_object` 本来就强制单个 JSON 值,于是任务无声地输了,连个错误都
看不到;根本没有展示过形状的那次,模型自创了一个对象(`sentiment`、`text`、`details`),
被契约拒绝——这正是上面那段引导语防止的事故。**内容**类要求则两不冲突:要求中文时字段值
就是中文,照样通过。若模型原生支持 `json_schema`,这些都不需要:端点会强制形状。

响应始终在本地对照同一份契约校验,所以
provider 被告知的内容与实际强制执行的内容不会各行其是:

```go
summary := loom.String("summary").Required().MinLen(1).MaxLen(200).Desc("Summary of the result.")
contract := loom.MustArgsContract("summary_result", summary)

args, response, err := loom.ChatStructuredArgs(ctx, "summary", model, request, contract)
summary.Get(args)
```

无论模型是按 `json_schema` 约束、按 `json_object` 请求约束,还是在能力未声明时不受
约束,每个响应都必须是满足契约的完整 JSON 值。Markdown 代码围栏、前后包裹的散文、
多个 JSON 值、未知参数和
约束违例都会被拒绝;JSON 空白符可以接受。不需要任何 strict 模式选项。schema 无法
表达的业务规则请用 `WithStructuredValidator`。

非法响应——JSON 非法、约束违例或业务校验失败——会作为 `*StructuredOutputError` 带着那次
响应内容返回,同时附上该次调用产生的同一个 `*ChatResponse`。Loom 只问一次:再试几次、
下一次请求带什么,是调用方的策略;ReAct 轮次的做法就是把这条错误和它已知的其它信息
一起放进对话。

### 重试一次结构化调用

重试必须由调用方做,原因在“重试的关键不是再问一次,而是第二次带什么”——被拒的输出、
被拒的原因,以及调用方手上那些上下文。`json_object` 模式下第一次的提示词就必须提到
JSON 并给出样例(供应商可能不提到就 400),而 schema 与契约样例通常是**重试时**才补
进去的。两样都在手里,所以循环就是几行:

```go
schemaJSON, _ := jsonv2.Marshal(contract.Schema())
messages := base
for attempt := 1; ; attempt++ {
    args, _, err := loom.ChatStructuredArgs(ctx, "summary", model,
        loom.ChatRequest{Messages: messages}, contract)
    if err == nil {
        break
    }
    var invalid *loom.StructuredOutputError
    if !errors.As(err, &invalid) || attempt == 3 || ctx.Err() != nil {
        return err // 请求本身的失败,provider 层已经重试过了
    }
    messages = append(messages,
        loom.Message{Role: loom.RoleAssistant, Content: invalid.Content},
        loom.Message{Role: loom.RoleUser, Content: fmt.Sprintf(
            "Your previous output was rejected: %v\nReturn one JSON value matching:\n%s",
            err, schemaJSON)})
}
```

走到 `*StructuredOutputError` 意味着模型给出了回答、但那个回答不可用;其它错误都来自请求
本身,而 provider 的重试调度已经在那里做过它该做的事。只有调用方能区分两者,也只有
调用方能决定下一次是原样重发、补上 schema,还是放弃。

### 两个方向共享的 schema

两个方向都建立在同一套 schema 模型上。`loom.Schema` 只承载声明式参数构建器会产出的
那些关键字,解码时拒绝任何其它关键字,所以一份 schema 不可能宣称一条校验器会悄悄
忽略的约束。

有些约束不是通过关键字本身,而是通过投影出来的 `pattern` 强制的:`Date`、`Time`、
`DateTime`、`UUID` 既设置 `format`,也设置匹配的形状 pattern;而只给 `format` 不给
pattern 会在构建契约时被拒。有一处刻意偏离规范:`integer` 必须写成不带小数和指数的
形式,因此 `1.0` 会被拒绝。

`loom.ValidateSchema(schema, value)` 对别处产生的值执行与契约相同的校验;
`loom.ConstJSON(v)` 把一个值渲染成 `const` 所持有的原始 JSON。

## 包

- `github.com/loomagent/loom`:运行时、事件、Writer、Sink、工具和模型抽象
- `github.com/loomagent/loom/handlerregistry`:并发、显式的 handler 注册
- `github.com/loomagent/loom/modelfactory`:与存储无关的模型构建与配置加载
- `github.com/loomagent/loom/modelprobe`:基于真实行为的模型能力探测与声明比对
- `github.com/loomagent/loom/contextpolicy`:可组合的上下文构建与审计决策
- `github.com/loomagent/loom/react`:与 provider 无关的 ReAct 运行时与策略接口
- `github.com/loomagent/loom/react/review`:通用 ReAct 质量闸门
- `github.com/loomagent/loom/prompttemplate`:显式的提示词占位符校验与渲染
- `github.com/loomagent/loom/sourceregistry`:与存储无关的源去重与稳定引用 ID
- `github.com/loomagent/loom/sourceregistry/sourceregistrytest`:可复用的 Store 一致性测试套件
- `github.com/loomagent/loom/providers/ark`:火山引擎 Ark provider
- `github.com/loomagent/loom/providers/deepseek`:DeepSeek provider
- `github.com/loomagent/loom/providers/openrouter`:OpenRouter provider
- `github.com/loomagent/loom/providers/serper`:Serper 网络搜索 provider
- `github.com/loomagent/loom/providers/unifuncs`:Unifuncs 文档读取 provider
- `github.com/loomagent/loom/tools/web/sourcedate`:与 provider 无关的发布日期提取
- `github.com/loomagent/loom/tools/web`:与 provider 无关的搜索与读取契约
- `github.com/loomagent/loom/tools/calculator`:沙箱化的 Starlark 计算器
- `github.com/loomagent/loom/tools/gettime`:固定北京时间的工具

架构与其背后的设计决策记录在 [DESIGN.md](DESIGN.md) 中,另有
[英文版](DESIGN.en.md)。

## 模型工厂

`modelfactory` 显式选择 provider,不会从 URL 反推。它接受普通的 Go 配置,且不依赖
数据库或 ORM:

```go
model, err := modelfactory.Build(modelfactory.Config{
	Provider: modelfactory.ProviderOpenRouter,
	APIKey:   os.Getenv("OPENROUTER_API_KEY"),
	Model:    "openai/gpt-5",
})
```

`Config.AttemptLimiter` 对**每一次物理尝试**做节流,这是宿主从外部做不到的事:重试循环在
loom 内部,所以包一层 `ChatModel` 只能限住逻辑调用,拦不下某次重试里的单次尝试。框架**不提供
实现**——一个凭据能有多少请求在飞、provider 推回时如何调整,是部署策略。`QuotaKey` 标识共享
该窗口的凭据(传指纹而不是密钥本身),`QuotaLabel` 是给指标用的低基数名字。

按 ID 选择模型的应用可以实现 `modelfactory.ConfigLoader` 并使用
`modelfactory.Factory`。加载器可以从文件、环境变量、密钥管理服务或数据库读取,
而不必让 Loom 耦合到那套存储系统。

## 模型能力探测

`modelprobe` 观察真实的 API 行为,而不是相信配置。它会测试默认推理行为和显式推理
行为、被接受的推理强度取值,以及原生 JSON object 与 JSON Schema 输出。报告会区分
正向、负向和操作上无法结论的检查,携带带版本号的 JSON schema,并且可以在没有数据库
的情况下与声明的 `loom.ModelCapabilities` 比对:

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

默认情况下,请求出错会保持"无法结论",绝不会悄悄变成"不支持"这一能力结论。
当 provider 能给出可靠的"参数不支持"错误分类时,应用可以提供 `ErrorClassifier`。
即使提供了该分类器,适配器本地产生的校验错误仍然保持无法结论。

Object 与 Schema 是互相独立的检查。Schema 探测只把一条新生成的随机约束放进所提供的
schema,并在正常 `stop` 之后校验完整响应;被截断或异常终止的输出属于无法结论。
报告会保留请求过的 schema 和响应模型以供审计。一次通过的采样证明的是该次请求与输出
成立,而不是每个 JSON Schema 关键字都被强制执行。即使另一项检查无法结论,
`Observed.StructuredOutput` 仍保留观察到的最强成功结果;而
`Coverage.StructuredOutput` 要求两项检查都能结论。解读汇总之前请先看每一项检查。
如果 `Probe` 在开始之后返回错误,请保留它的部分报告:已完成的检查会被保留,而取消
会阻止后续实验。

探测候选只来自管理员配置的 `Options.DeclaredCapabilities`。
`Options.ReasoningEfforts` 可以显式收窄诊断候选;传空切片即跳过。
不存在内置的模型目录或别名映射。报告会分别记录配置的候选、已测/未测/未解析的强度、
实际线上参数、请求是否被接受,以及返回的推理证据。`Complete` 和
`CandidateCoverageComplete` 的含义是配置的候选都被测过了,它们并不认证原生语义。
历史报告保留其记录时的事实。

`ReasoningEffort` 是开放的、provider 原生的字符串类型。管理员必须填写其 provider 与
模型版本真正支持的准确取值。业务侧的可选项只来自
`ModelCapabilities.ReasoningEfforts`;Go 常量只是便利取值,不是全局白名单。取值区分
大小写,永不做归一化、别名、排序,也不转换成 token 预算。适配器原样传输所选值,
包括 SDK 发布之后才引入的取值。

保存时用 `ValidateModelReasoningCapabilities`,请求时用 `ResolveModelReasoning`。
每个请求都要求显式的推理开关。当模型声明了强度档位时,启用推理必须显式选择强度;
关闭推理则拒绝任何强度。已确认但不可调强度的模型只使用那个显式开关。未经人工复核的
导入模型记录必须设置 `ReasoningEffortsUnconfirmed: true`,它们不能启用推理。
`OfficialDefaultReasoningEffort` 记录复核过的官方默认值。提供时它必须属于已声明的
强度列表。它既不会补上缺失的任务选择,也不会改变已有选择。只有隔离的
`ReasoningProbeCapabilities` 模型才会刻意省略这些控制来观察 provider 默认行为。
探测结果永不改变业务声明。

## 提示词模板

`prompttemplate` 在渲染前校验必需的占位符恰好出现一次。内置占位符覆盖用户输入、
助手回答和对话上下文;调用方也可以使用任意占位符字符串。

## 来源日期

`tools/web/sourcedate` 从 Markdown 文档顶部保守地提取发布日期。它能识别带标签的
和独立的英文、ISO 与中文日期格式,同时拒绝明显过早或将来的日期。结果包含原始文本、
解析出的 UTC 日期、证据来源和置信度。

该包只解析文档内容。搜索 provider 的元数据和来源注册表的策略刻意留在该包之外。

## 来源注册表

`sourceregistry` 在一个会话或其它命名空间内分配稳定的 `SRC-N` 引用。它会归一化 URL、
在批次内和跨批次去重、按首次出现的顺序连续分配新引用、保留元数据的来源信息,并以
单调方式升级全文可用性:

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

上面两条观察共享同一个序列和同一条内容路径。`Created` 只在第一条输入上为真,因此
计数时不会重复统计。应用可以把 `MemoryStore` 换成事务型数据库 Store。Store 契约
要求按命名空间线性一致、URL 与序列键唯一、分配连续、结果有序,以及批次全有或全无。
自定义的 URL 归一化器可以实现移除跟踪参数之类的产品特定规则。

数据库适配器可以跑与 `MemoryStore` 相同的一致性测试套件:

```go
func TestStoreContract(t *testing.T) {
	sourceregistrytest.TestStore(t, func(t *testing.T) sourceregistry.Store {
		return newTestStore(t)
	})
}
```

该套件检查空批次、有序且连续的分配、元数据合并、命名空间隔离、返回值的别名问题、
被取消的事务,以及不同来源与相同来源并发注册时的线性一致性。

## ReAct 运行时

`react.Run` 负责流式模型调用、工具执行、单工具预算与总预算、结束原因,以及最后
一次不带工具的软着陆。应用可以通过三个小的策略接口扩展它,而不必 fork 循环:

- `StepPolicy` 在调用前调整上下文、可见工具和工具选择。
- `AfterToolsPolicy` 审阅结果、改变上下文,或叫停循环。
- `FinishPolicy` 接受或拒绝模型尝试结束。

`contextpolicy.ReactStepPolicy` 把可组合的上下文构建器适配到该循环。
`react/review.Policy` 提供有状态的质量闸门,而把真正的评审方、评审标准和指令留给
应用。

### ReAct 循环里的结构化输出

有两种惯用形状,而且都不需要手写重试循环:

- **用一个终止工具承载契约。** 把循环必须调用来结束的那个工具的**参数**写成契约:
  参数不满足契约的调用会作为 tool result 回到模型,并说明期望什么
  (`expected arguments: …`),循环就带着它在上下文里继续下去——schema 本来就已经在
  `tools[].function.parameters` 里了,不需要说第二遍。
- **用 `FinishPolicy` 接受或拒绝结束。** `react/review.Policy` 就是现成范例:评审不满足时
  返回 `react.FinishDecision{Continue: true, Instruction: …}`,把“模型想停”变成带着反馈
  多跑一轮。契约校验是同一个形状:

```go
func (p answerContract) BeforeFinish(_ context.Context, _ react.State, resp *loom.ChatResponse) (react.FinishDecision, error) {
    if _, err := p.contract.Decode(resp.Content); err == nil {
        return react.FinishDecision{}, nil // 回答满足契约
    }
    return react.FinishDecision{Continue: true, Instruction: "Answer with the required JSON object."}, nil
}
```

两种做法都把反馈留在对话里、留在调用方看得见的地方,而不是放在一个由框架改写的请求里。

## Web 工具

`tools/web` 包定义了归一化的 `WebSearcher` 与 `WebReader` 接口,以及 Loom 的工具
包装。provider 实现自己负责网络访问、缓存、鉴权和重试。公开的工具层不分配引用 ID、
不持久化文档,也不依赖某个搜索厂商。

Serper 和 Unifuncs 作为可选的 provider 实现提供。它们的客户端直接满足这些与
provider 无关的接口:

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

Unifuncs provider 包含请求限流、对瞬时故障的有界重试、`Retry-After` 处理,以及
发布日期提取。Serper provider 把厂商的日期和结果位置字段保留为结果元数据。

## 内置实用工具

`tools/calculator` 在受限的 Starlark 环境里求值,并使用普通的 JSON 请求/响应
结构体。`tools/gettime` 刻意在 UTC 之外返回固定的 Asia/Shanghai 本地时间;模型
无法通过工具参数覆盖时区。

## 项目状态

Loom 正在积极开发中。在第一个稳定版发布之前,API 可能在小版本之间变化。生产环境
使用者应当锁定精确版本。

## 开发

```bash
gofmt -l .
go vet ./...
go test -race ./...
golangci-lint run ./...
```

CI 会执行以上全部命令。`golangci-lint` 承载了项目测试原则所要求的 lint 规则:
`depguard`(测试使用 `encoding/json/v2`)、`forbidigo`(测试用 `synctest` 推进时间,
绝不 `time.Sleep`)、`usetesting`、`thelper`、`tparallel`、`modernize`,以及
`unused`、`unparam`、`unconvert`(不保留没人读的声明、参数和无意义的类型转换)。

测试步骤还会写出覆盖率 profile,由 `scripts/check-coverage.sh` 对照一个下限检查。
这个下限是防止回退的棘轮,而不是分支覆盖率目标:`go test -cover` 只统计各包自己的
测试,所以总量会低估那些被其它包调用的 API。

### 真实端点测试

`livetest/` 让 Loom 打真实 provider 端点。把 `live.env.example` 复制到仓库之外的
地方，为每家 provider 填上端点、key 和一到多个模型，再把套件指过去：

```bash
LOOM_LIVE_ENV=~/live.env go test ./livetest/
```

这个路径是唯一的开关：没有默认位置，所以 `go test ./...` 永远不会碰到 provider、
不会意外花掉凭据，CI 也不需要任何凭据。

一个块声明一家 provider 并列出它的模型，一份凭据就能覆盖你想观察的任意多个模型：

```ini
LOOM_LIVE_PROVIDERS=deepseek,openrouter
LOOM_LIVE_DEEPSEEK_URL=https://api.deepseek.com/v1
LOOM_LIVE_DEEPSEEK_KEY=...
LOOM_LIVE_DEEPSEEK_MODELS=<model>[,<model>...]
```

每个模型都跑同样三条链路:一轮流式工具调用(其参数会按工具自己的契约解码)、
把该 assistant 轮回传进第二次请求、以及一次结构化输出调用。这三条是测试断言的对象;
随模型而变的东西——拿回多少推理文本、是否以结构化块返回——只记日志不断言。每家
provider 是一个子测试、其下每个模型又是一个子测试,因此
`-run TestLiveProviders/<provider>` 选中一家,`-run
TestLiveProviders/<provider>/<model>` 选中一个模型。

没有 `LOOM_LIVE_ENV` 时该测试整体跳过，所以 `go test ./...` 保持自足、CI 不需要任何凭据。
`livetest/env_test.go` 覆盖了文件解析和它报出的每一种配置错误，并且在
`live.env.example` 或文档里出现具体模型名时会失败：示例只承载配置的形状，选择留给
操作者——一份凭据能调用哪些模型，是操作者的决定。

在本仓库里，key 存在 Actions secrets，端点与模型列表存在 Actions variables；
`.github/workflows/live.yml` 按需手动运行：不在 push 或 pull request 上运行，因为手动或
定时触发用到的是默认分支上的那份工作流文件，分支无法用改了代码的工作流拿到仓库凭据。

`internal/toolcontract` 会用官方
[JSON Schema Test Suite](https://github.com/json-schema-org/JSON-Schema-Test-Suite)
校验它所支持的子集:draft2020-12 中与已支持关键字对应的文件被放在
`internal/toolcontract/testdata/jsonschema` 下,而 schema 无法解码进 Loom 模型的
用例会被跳过。当某个已记录的偏差被修好却没有同步更新那张表时,
`TestJSONSchemaSuiteSubset` 会失败,以此保证那张表始终是最新的。要刷新 fixtures,
请从该目录 README 中记录的上游 commit 取文件。

### 文档语言

README 保持中英双语:本文件是中文版,[README.md](README.md) 是英文版。设计文档
同样如此:[DESIGN.md](DESIGN.md) 是中文原文,[DESIGN.en.md](DESIGN.en.md) 是
英文版。每个文件都以指向另一语言版本的链接开头,并且两份在同一修改中一起更新,
读者不会落在一份过期的翻译上。

## 智谱 AI(国内按量付费 API)

使用 `modelfactory.ProviderZhipuAI`(`zhipuai`)或 `providers/zhipuai.New`。
默认端点是 `https://open.bigmodel.cn/api/paas/v4`;该集成覆盖国内 Chat Completions
API,不覆盖 Z.AI 或 Coding Plan。

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

本例中的能力由调用方提供;声明为 nil 时会转发显式控制以便探测,而不是按模型名
硬编码行为。该适配器在 Chat 和 Stream 中都会传输 JSON Object 和完整的 JSON Schema
请求,包括 schema 名、描述、约束和 `strict:true`。业务侧声明的能力仍然会拦截请求;
探测会绕过这些闸门,以观察端点当前的真实行为。Schema 请求永远不会被降级,也不会被
替换成提示词指令。强制选择工具会返回 `loom.ErrUnsupportedCapability`。它在工具
调用之间保留 `reasoning_content`,并为流式工具请求启用 `tool_stream`。缺少推理
token 遥测并不能证明思考被关闭(`Usage.ReasoningTokensKnown` 区分"没有遥测"和
"显式为零")。

SDK 重试已关闭,由 Loom 掌握重试预算。`zhipuai.APIError` 保留 HTTP 状态和业务
code;1113 这类余额错误立即失败,1302 走有界的限流退避,1305 走有限次的瞬时故障
重试。参见 [智谱 API 文档](https://docs.bigmodel.cn/cn/api/introduction) 和
[GLM-5.3](https://docs.bigmodel.cn/cn/guide/models/text/glm-5.3)。

推理能力有三种状态:`none`、`always_on` 和 `toggleable`。历史上的
`toggleable_default_on/off` 取值仍然接受,并通过 `ReasoningSupport.Canonical()`
归一化。应用请求会显式选择推理;当声明了强度档位时,启用推理还必须显式给出受支持的
强度。探测报告会单独保留对服务端默认行为的观察。
