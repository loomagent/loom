package loom

import "time"

// TurnStatus 一次 Turn 执行的生命周期状态。
//
// 状态转换:
//
//	queued ──▶ in_progress ──┬──▶ completed
//	                         ├──▶ cancelled
//	                         └──▶ failed
//
// queued 状态由外部(如 dispatcher 预创建 Turn 行)使用;loom.Run 直接从
// in_progress 起步,跑完落入终态(completed / cancelled / failed)。
type TurnStatus string

const (
	TurnStatusQueued     TurnStatus = "queued"
	TurnStatusInProgress TurnStatus = "in_progress"
	TurnStatusCompleted  TurnStatus = "completed"
	TurnStatusCancelled  TurnStatus = "cancelled"
	TurnStatusFailed     TurnStatus = "failed"
)

// CloseReason Turn 关闭原因的完整描述。
//
// 用法:
//   - 出现在 *CloseReason 字段中,nil 表示 Turn 仍在运行
//   - Turn.Status 表达生命周期大类(completed/cancelled/failed)
//   - Code 表达终态原因
//
// UI / 监控可按 Status 做大类聚合,按 Code 做细分路由:
//   - "timeout" / "content_filter" 等
//   - 内置常量见 CloseCode* 系列
type CloseReason struct {
	Code    CloseCode // 细分原因
	Message string    // 人类可读说明
	Cause   error     // 原始错误(失败类填,可 nil)
}

type CloseCode string

// 内置 CloseReason.Code 常量。CloseCode 是封闭枚举,新增值时需要同步
// proto/agent/v2.CloseCode、Ent loom_turn.close_code 和 StatusFromCloseCode。
const (
	// completed
	CloseCodeFinalAnswer CloseCode = "final_answer" // executor 写过 final_answer

	// cancelled
	CloseCodeUserCancel     CloseCode = "user_cancel"     // 用户主动取消(StopGeneration / ctx.Cancel)
	CloseCodeTimeout        CloseCode = "timeout"         // ctx.DeadlineExceeded
	CloseCodeHostShutdown   CloseCode = "host_shutdown"   // runtime host 优雅停止
	CloseCodeExternalCancel CloseCode = "external_cancel" // 外部权威控制面要求取消

	// failed
	CloseCodeAgentError      CloseCode = "agent_error"      // handler return error
	CloseCodeContentFilter   CloseCode = "content_filter"   // LLM 内容审核
	CloseCodeOutputTruncated CloseCode = "output_truncated" // LLM 输出撞 max_tokens / 模型上限被截断
	CloseCodeNoFinal         CloseCode = "no_final_answer"  // return nil 但没写 final_answer
	CloseCodeInternalError   CloseCode = "internal_error"   // runtime host 内部异常 / orphan recovery
)

// Turn 一次 agent 执行的完整数据(用户提问 + agent 全部过程 + 最终回答)。
//
// 两种角度共用此类型:
//   - loom.Run 返回的快照(执行后)
//   - Repository.LoadHistory 加载的历史 Turn(用于拼下一轮上下文)
//
// 完整可序列化为 JSON,供前端直接渲染。
type Turn struct {
	// Index 在 conversation 内的 0-based 序号(0 = 第一个 turn)。
	Index uint64
	// Path 派生为 "turn[N]",方便引用与 log。
	Path string
	// ConversationID 本 turn 归属的 conversation id(一等公民,Run 校验非空)。
	ConversationID string

	// Items 完整 item 树(嵌套结构,step 通过 Children 持有子项)。
	// 顶层一般顺序:user_message → reasoning/step/note/tool_call/tool_result → final_answer。
	Items []Item

	Status      TurnStatus
	CloseReason *CloseReason // nil = 仍在运行(仅 Repository 加载未完成 Turn 时出现)
	Usage       Usage        // 整 Turn 累计 token 用量(各次 LLM 调用聚合)

	// Metadata 业务侧透传 K/V(loom 不解释)。
	// 常见 key 约定:"conversation_id" / "turn_index" / "user_id" / "chat_mode" / ...
	Metadata map[string]string

	CreatedAt time.Time
	UpdatedAt time.Time
}
