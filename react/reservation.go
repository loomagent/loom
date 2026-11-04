package react

import "context"

// ToolReservation identifies one runtime-accepted call, without arguments or
// result contents. Index is its zero-based position in the original model
// response, including positions occupied by rejected calls. CallID alone is
// not a durable identity; hosts must also bind their execution scope and Step.
type ToolReservation struct {
	Index         uint64
	CallID        string
	Name          string
	BudgetExempt  bool
	EndsToolPhase bool
}

// ToolReservationBatch is a value snapshot after reserving the whole accepted
// batch and before execution starts. Step follows State.Step's zero-based
// main-model index. Business counts include failed or not-yet-started calls;
// exempt calls, including terminal calls, do not enter these counts.
// ToolUses is the per-business-tool count, not a count of all executions.
// Reservations includes accepted internal and terminal calls in model order.
// A host may retain or change the snapshot without changing runtime state.
type ToolReservationBatch struct {
	Step           uint64
	ToolCallsUsed  uint64
	ToolRoundsUsed uint64
	ToolUses       map[string]uint64
	Reservations   []ToolReservation
}

// ToolBatchReserver persists or otherwise admits the entire accepted batch.
// It is called synchronously once per nonempty accepted batch, with the run's
// cancellation and deadline. Refused calls are excluded. In particular, a
// mixed terminal batch and a wholly refused batch never reach this callback.
// Return success only after all reservations are durable if persistence is
// required. A failure can leave conservative reservations but no batch side
// effects. This hook neither restores budgets nor reports execution outcomes.
type ToolBatchReserver func(context.Context, ToolReservationBatch) error
