package loom

import (
	"fmt"
	"strings"
)

// HistoryToMessages turns the history turns plus this round's input into a sequence
// of LLM Messages that can be handed straight to a ChatModel.
//
// Rules, walking the Turn.Items tree depth first:
//   - user_message  → Message{Role: RoleUser, Content: Text}
//   - reasoning     → accumulated, and folded into the ReasoningContent of the first
//     assistant message that follows it
//   - tool_call     → accumulated, and merged into one assistant message when the
//     next tool_result or final_answer appears
//   - tool_result   → Message{Role: RoleTool, ToolCallID, Content: Output}
//   - final_answer  → Message{Role: RoleAssistant, Content: Text}, carrying the
//     accumulated ReasoningContent
//   - note / step   → absent from the conversation fed to the model, since process
//     information is not something the model needs
//
// Reasoning pairs with the first assistant message after it, so the model sees
// "reasoning plus that round's output" in the same shape it produced. Several
// reasoning blocks before one assistant message are joined with "\n\n".
//
// This round's input is appended last as a user message.
func HistoryToMessages(history []Turn, input UserMessage) ([]Message, error) {
	var out []Message
	for i := range history {
		if err := validateHistoryTurnForMessages(&history[i]); err != nil {
			return nil, err
		}
		out = append(out, turnToMessages(&history[i])...)
	}
	out = append(out, NewTaskUserMessage(input.Source, input.Text))
	return out, nil
}

func validateHistoryTurnForMessages(turn *Turn) error {
	switch turn.Status {
	case TurnStatusCompleted:
		return nil
	case TurnStatusQueued:
		return fmt.Errorf("history turn[%d] has status=%s and has not finished; it cannot form LLM context", turn.Index, turn.Status)
	case TurnStatusInProgress:
		return fmt.Errorf("history turn[%d] has status=%s and is still running; it cannot form LLM context", turn.Index, turn.Status)
	case TurnStatusCancelled:
		return fmt.Errorf("history turn[%d] has status=%s and did not end cleanly; it cannot form LLM context", turn.Index, turn.Status)
	case TurnStatusFailed:
		return fmt.Errorf("history turn[%d] has status=%s and did not end cleanly; it cannot form LLM context", turn.Index, turn.Status)
	default:
		return fmt.Errorf("history turn[%d] has unknown status=%q; it cannot form LLM context", turn.Index, turn.Status)
	}
}

// turnToMessages turns one Turn into a list of LLM Messages.
func turnToMessages(turn *Turn) []Message {
	var out []Message
	var pendingCalls []ToolCall
	var pendingReasoning strings.Builder

	// appendAssistantMsg builds an assistant message, folding in pendingReasoning and
	// clearing it.
	appendAssistantMsg := func(content string, calls []ToolCall) {
		msg := Message{
			Role:    RoleAssistant,
			Content: content,
		}
		if len(calls) > 0 {
			msg.ToolCalls = calls
		}
		if pendingReasoning.Len() > 0 {
			msg.ReasoningContent = pendingReasoning.String()
			pendingReasoning.Reset()
		}
		out = append(out, msg)
	}

	// flushCalls folds the accumulated tool calls into one assistant message.
	flushCalls := func() {
		if len(pendingCalls) > 0 {
			appendAssistantMsg("", pendingCalls)
			pendingCalls = nil
		}
	}

	walkItems(turn.Items, func(it *Item) {
		switch it.Kind {
		case ItemKindUserMessage:
			flushCalls()
			// Orphaned reasoning before a user message is rare and means a logic error;
			// emit it as an assistant message rather than lose it
			if pendingReasoning.Len() > 0 {
				appendAssistantMsg("", nil)
			}
			out = append(out, NewTaskUserMessage(it.MessageSource, it.Text))

		case ItemKindReasoning:
			if pendingReasoning.Len() > 0 {
				pendingReasoning.WriteString("\n\n")
			}
			pendingReasoning.WriteString(it.Text)

		case ItemKindToolCall:
			pendingCalls = append(pendingCalls, ToolCall{
				ID:        it.ToolCallID,
				Name:      it.ToolName,
				Arguments: it.Arguments,
			})

		case ItemKindToolResult:
			flushCalls() // fold the accumulated tool calls into an assistant message first
			out = append(out, Message{
				Role:       RoleTool,
				ToolCallID: it.ToolCallID,
				Content:    it.Output,
			})

		case ItemKindFinalAnswer:
			// Only a committed answer belongs in the history. A failed or cancelled attempt is an
			// audit record: feeding its partial text to the model would present a draft the run
			// refused, and the next Turn cannot tell it apart from the answer.
			if it.Status != ItemStatusCompleted {
				return
			}
			flushCalls()
			appendAssistantMsg(it.Text, nil)
		case ItemKindStep:
			// A step is a container; walkItems descends into its children.
		default:
			// An unknown kind stays out of the LLM history, so process data the model
			// cannot interpret is never fed to it.
		}
	})

	flushCalls()
	// The final catch: reasoning with nothing after it, no final answer and no tool
	// call
	if pendingReasoning.Len() > 0 {
		appendAssistantMsg("", nil)
	}
	return out
}

// walkItems walks the Items tree depth first.
func walkItems(items []Item, fn func(*Item)) {
	for i := range items {
		fn(&items[i])
		if len(items[i].Children) > 0 {
			walkItems(items[i].Children, fn)
		}
	}
}

// AppendAssistantTurn appends one round of LLM response plus its tool results to the
// end of messages, following the LLM protocol.
//
// The shape it builds, ready for the next call:
//   - one assistant message carrying Content, ReasoningContent, and ToolCalls
//   - one tool message per tool call, with Content set to the output or to the error
//     description
//
// Product code calls it inside a ReAct-style loop instead of hand-writing that
// boilerplate.
//
// A failed tool: when r.Err is non-nil the tool message's Content becomes
// "tool execution error: <err>", so the model sees why it failed and decides whether
// to switch tools or to retry.
func AppendAssistantTurn(msgs []Message, resp *ChatResponse, results []ToolExecResult) []Message {
	msgs = append(msgs, Message{
		Role:             RoleAssistant,
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		// A provider's structured reasoning continues the chain it came from, so it goes
		// back on the message that continues it.
		ReasoningDetails: resp.ReasoningDetails,
		ToolCalls:        resp.ToolCalls,
	})
	for _, r := range results {
		content := r.Output
		if r.Err != nil && content == "" {
			content = "tool execution error: " + r.Err.Error()
		}
		msgs = append(msgs, Message{
			Role:       RoleTool,
			ToolCallID: r.Call.ID,
			Content:    content,
		})
	}
	return msgs
}
