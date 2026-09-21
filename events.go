package loom

import "time"

// DeltaChannel is the channel a streaming increment belongs to. Each ItemKind
// uses its own:
//   - streaming reasoning / note / final_answer → Text
//   - streaming tool_call arguments → Arguments
//   - streaming tool_result output → Output
type DeltaChannel string

const (
	DeltaChannelText      DeltaChannel = "text"
	DeltaChannelArguments DeltaChannel = "arguments"
	DeltaChannelOutput    DeltaChannel = "output"
)

// ItemStartedEvent reports that an Item first appeared.
//
// Item.Status is usually InProgress, on the streaming open path, or Completed, on
// the one-shot write path. A Sink routes on Item.Kind, typically with a type
// switch.
type ItemStartedEvent struct {
	TurnIndex uint64
	TurnPath  string
	Item      Item // the complete Item, in its initial state
	Time      time.Time
}

// ItemDeltaEvent reports one streaming increment. Only the streaming write paths
// produce it, such as StreamReasoning and StreamFinalAnswer.
//
// Channel says which kind of increment this is, and Chunk accumulates into the
// matching Item field. A Sink may keep that accumulated state itself, or keep
// none and wait for the final value in ItemFinishedEvent.
type ItemDeltaEvent struct {
	TurnIndex uint64
	TurnPath  string
	ItemPath  string // the full path that locates the Item
	Channel   DeltaChannel
	Chunk     string
	Time      time.Time
}

// ItemFinishedEvent reports that an Item reached its final state.
//
// Item.Status is Completed, Cancelled, or Failed, and Error is filled in
// accordingly. A streamed Item carries its final accumulated text here, or the
// value given to SetFinalText.
type ItemFinishedEvent struct {
	TurnIndex uint64
	TurnPath  string
	Item      Item // the complete Item in its final state
	Time      time.Time
}

// LLMCalledEvent reports that one LLM call finished, with its token usage and
// where it belongs.
//
// StepPath is the step this call belongs to; empty means it hangs off the turn
// root. The framework core or a helper such as StreamLLMToStep emits it when the
// call returns.
type LLMCalledEvent struct {
	TurnIndex uint64
	TurnPath  string
	StepPath  string // the owning step (empty = the turn root)
	Model     string // provider/model id
	Purpose   string // free text for debugging and auditing ("react.turn3", "researcher.plan", ...)
	Usage     Usage
	Time      time.Time
}
