package loom

import "time"

// TurnStatus is the lifecycle state of one Turn execution.
//
// Transitions:
//
//	queued ──▶ in_progress ──┬──▶ completed
//	                         ├──▶ cancelled
//	                         └──▶ failed
//
// Callers that create a Turn row ahead of time, such as a dispatcher, use the
// queued state. loom.Run starts at in_progress and ends in one of the terminal
// states.
type TurnStatus string

const (
	TurnStatusQueued     TurnStatus = "queued"
	TurnStatusInProgress TurnStatus = "in_progress"
	TurnStatusCompleted  TurnStatus = "completed"
	TurnStatusCancelled  TurnStatus = "cancelled"
	TurnStatusFailed     TurnStatus = "failed"
)

// CloseReason describes in full why a Turn closed.
//
// How it is used:
//   - it appears in a *CloseReason field, where nil means the Turn is still running
//   - Turn.Status carries the broad lifecycle class (completed/cancelled/failed)
//   - Code carries the specific reason for that terminal state
//
// A UI or monitor aggregates by Status and routes by Code:
//   - "timeout", "content_filter", and so on
//   - the built-in constants are the CloseCode* values
type CloseReason struct {
	Code    CloseCode // the specific reason
	Message string    // a human-readable explanation
	Cause   error     // the original error, set for failures and possibly nil
}

type CloseCode string

// The built-in CloseReason.Code constants. CloseCode is a closed enumeration:
// adding a value means updating proto/agent/v2.CloseCode, the Ent column
// loom_turn.close_code, and StatusFromCloseCode together.
const (
	// completed
	CloseCodeFinalAnswer CloseCode = "final_answer" // the handler wrote a final_answer

	// cancelled
	CloseCodeUserCancel     CloseCode = "user_cancel"     // cancelled by the user (StopGeneration or ctx.Cancel)
	CloseCodeTimeout        CloseCode = "timeout"         // ctx.DeadlineExceeded
	CloseCodeHostShutdown   CloseCode = "host_shutdown"   // the runtime host is shutting down gracefully
	CloseCodeExternalCancel CloseCode = "external_cancel" // an authoritative external control plane required cancellation

	// failed
	CloseCodeAgentError      CloseCode = "agent_error"      // handler return error
	CloseCodeContentFilter   CloseCode = "content_filter"   // the model's content moderation
	CloseCodeOutputTruncated CloseCode = "output_truncated" // the output hit max_tokens or the model's own limit
	CloseCodeNoFinal         CloseCode = "no_final_answer"  // the handler returned nil without writing a final_answer
	CloseCodeInternalError   CloseCode = "internal_error"   // an internal runtime-host failure, or orphan recovery
)

// Turn is the complete data of one agent execution: the user's question, the
// agent's whole process, and the final answer.
//
// Both angles share this type:
//   - the snapshot loom.Run returns once execution finishes
//   - a historical Turn loaded by Repository.LoadHistory, used to build the next
//     round of context
//
// It serializes to JSON in full, ready for a frontend to render.
type Turn struct {
	// Index is the 0-based position within the conversation; 0 is the first turn.
	Index uint64
	// Path is derived as "turn[N]", which makes it easy to reference and to log.
	Path string
	// ConversationID is the conversation this turn belongs to. It is a first-class
	// value, and Run requires it to be non-empty.
	ConversationID string

	// Items is the complete item tree; a step holds its children through Children.
	// The top level usually reads user_message →
	// reasoning/step/note/tool_call/tool_result → final_answer.
	Items []Item

	Status      TurnStatus
	CloseReason *CloseReason // nil = still running, which only a Repository loading an unfinished Turn sees
	Usage       Usage        // the Turn's accumulated token usage, summed over its LLM calls

	// Metadata is a K/V passthrough for product code; loom does not interpret it.
	// Conventional keys include "conversation_id", "turn_index", "user_id", and
	// "chat_mode".
	Metadata map[string]string

	CreatedAt time.Time
	UpdatedAt time.Time
}
