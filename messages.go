package loom

import (
	"fmt"
	"strings"
)

// HistoryToMessages 把历史 Turn 列表 + 本轮 input 转成 LLM Message 序列,可直接喂 ChatModel。
//
// 转换规则(按 Turn.Items 树深度优先遍历):
//   - user_message  → Message{Role: RoleUser, Content: Text}
//   - reasoning     → 累积,塞入紧随其后第一条 assistant message 的 ReasoningContent
//   - tool_call     → 累积,遇到下个 tool_result/final_answer 时合并成一条 assistant message
//   - tool_result   → Message{Role: RoleTool, ToolCallID, Content: Output}
//   - final_answer  → Message{Role: RoleAssistant, Content: Text}(带累积的 ReasoningContent)
//   - note / step   → 不出现在喂 LLM 的对话流(过程信息,LLM 不需要)
//
// reasoning 配对原则:紧跟它之后第一条 assistant message。这样 LLM 看到的
// "reasoning + 该轮产出" 跟原始 LLM 调用时的形态一致。多个 reasoning 在同一条
// assistant message 之前出现时,用 "\n\n" 拼接。
//
// 末尾追加本轮 input 作为 user message。
func HistoryToMessages(history []Turn, input UserMessage) ([]Message, error) {
	var out []Message
	for i := range history {
		if err := validateHistoryTurnForMessages(&history[i]); err != nil {
			return nil, err
		}
		out = append(out, turnToMessages(&history[i])...)
	}
	out = append(out, Message{Role: RoleUser, Content: input.Text})
	return out, nil
}

func validateHistoryTurnForMessages(turn *Turn) error {
	switch turn.Status {
	case TurnStatusCompleted:
		return nil
	case TurnStatusQueued:
		return fmt.Errorf("history turn[%d] status=%s,尚未执行完成,不能拼 LLM 上下文", turn.Index, turn.Status)
	case TurnStatusInProgress:
		return fmt.Errorf("history turn[%d] status=%s,仍在执行中,不能拼 LLM 上下文", turn.Index, turn.Status)
	case TurnStatusCancelled:
		return fmt.Errorf("history turn[%d] status=%s,未正确结束,不能拼 LLM 上下文", turn.Index, turn.Status)
	case TurnStatusFailed:
		return fmt.Errorf("history turn[%d] status=%s,未正确结束,不能拼 LLM 上下文", turn.Index, turn.Status)
	default:
		return fmt.Errorf("history turn[%d] status=%q 未知,不能拼 LLM 上下文", turn.Index, turn.Status)
	}
}

// turnToMessages 把单个 Turn 转成 LLM Message 列表。
func turnToMessages(turn *Turn) []Message {
	var out []Message
	var pendingCalls []ToolCall
	var pendingReasoning strings.Builder

	// appendAssistantMsg 构造 assistant message,自动塞入 pendingReasoning 并清空。
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

	// flushCalls 把累积的 tool_calls 收成一条 assistant message。
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
			// user 前若有孤儿 reasoning(罕见,逻辑异常),作 assistant message 输出避免丢失
			if pendingReasoning.Len() > 0 {
				appendAssistantMsg("", nil)
			}
			out = append(out, Message{Role: RoleUser, Content: it.Text})

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
			flushCalls() // 先把累积 tool_calls 收成 assistant message(带 reasoning)
			out = append(out, Message{
				Role:       RoleTool,
				ToolCallID: it.ToolCallID,
				Content:    it.Output,
			})

		case ItemKindFinalAnswer:
			flushCalls()
			appendAssistantMsg(it.Text, nil)
		case ItemKindStep:
			// step 是容器,walkItems 会继续遍历它的 children。
		default:
			// 未知 kind 不进入 LLM 历史,避免把不可解释的过程数据喂给模型。
		}
	})

	flushCalls()
	// 末尾兜底:孤儿 reasoning(turn 没 final_answer 也没 tool_call)
	if pendingReasoning.Len() > 0 {
		appendAssistantMsg("", nil)
	}
	return out
}

// walkItems 深度优先遍历 Items 树。
func walkItems(items []Item, fn func(*Item)) {
	for i := range items {
		fn(&items[i])
		if len(items[i].Children) > 0 {
			walkItems(items[i].Children, fn)
		}
	}
}

// AppendAssistantTurn 把一轮 LLM 响应 + 工具执行结果按 LLM 协议拼到 messages 末尾。
//
// 拼出来的结构(供下一轮 LLM 调用):
//   - 1 条 assistant message(含 Content / ReasoningContent / ToolCalls)
//   - N 条 tool message(每个 tool_call 一条,Content = Output 或错误描述)
//
// 业务方在 ReAct 类循环内调用,代替手动 append 那段样板。
//
// 工具失败处理:r.Err 非 nil 时,tool message 的 Content 写
// "tool execution error: <err>",让 LLM 看到失败原因自己决定换工具/重试。
func AppendAssistantTurn(msgs []Message, resp *ChatResponse, results []ToolExecResult) []Message {
	msgs = append(msgs, Message{
		Role:             RoleAssistant,
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
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
