package loom

import "time"

// DeltaChannel 流式增量的通道类型。
// 不同 ItemKind 对应不同 channel:
//   - reasoning / note / final_answer 流式时 → Text
//   - tool_call 流式 arguments → Arguments
//   - tool_result 流式 output → Output
type DeltaChannel string

const (
	DeltaChannelText      DeltaChannel = "text"
	DeltaChannelArguments DeltaChannel = "arguments"
	DeltaChannelOutput    DeltaChannel = "output"
)

// ItemStartedEvent 一个 Item 首次出现。
//
// Item.Status 此时通常为 InProgress(流式 Open 路径)或 Completed(一次性 Write 路径)。
// Sink 实现按 Item.Kind 路由具体处理(走 type switch on Item.Kind)。
type ItemStartedEvent struct {
	TurnIndex uint64
	TurnPath  string
	Item      Item // 完整 Item 数据(此时为初始态)
	Time      time.Time
}

// ItemDeltaEvent 流式增量。
//
// 仅流式写入路径(StreamReasoning / StreamFinalAnswer / 等)产生。
// 调用方按 Channel 区分增量类型,Chunk 累加到 Item 的对应字段(由 Sink 实现自己维护累积态,
// 或不维护 — 等 ItemFinishedEvent 给最终值)。
type ItemDeltaEvent struct {
	TurnIndex uint64
	TurnPath  string
	ItemPath  string // 完整路径,定位 Item
	Channel   DeltaChannel
	Chunk     string
	Time      time.Time
}

// ItemFinishedEvent Item 进入终态。
//
// Item.Status 必为 Completed / Incomplete / Failed 之一,Error 字段按 Status 填充。
// 流式 Item 此时携带最终累积值(或 SetFinalText 给的覆盖值)。
type ItemFinishedEvent struct {
	TurnIndex uint64
	TurnPath  string
	Item      Item // 终态完整 Item
	Time      time.Time
}

// LLMCalledEvent 一次 LLM 调用完成,上报 token 用量与归因。
//
// StepPath 表示这次调用归属哪个 step(空 = 挂 turn 根)。
// agent 框架内核或 helper(如 StreamLLMToStep)在 LLM 调用完成时 emit。
type LLMCalledEvent struct {
	TurnIndex uint64
	TurnPath  string
	StepPath  string // 归属 step(空 = turn 根)
	Model     string // provider/model id
	Purpose   string // 调试/审计自由文本("react.turn3" / "researcher.plan" / ...)
	Usage     Usage
	Time      time.Time
}
