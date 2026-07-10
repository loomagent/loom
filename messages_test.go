package loom

import "testing"

func TestHistoryToMessages_ReasoningPairing(t *testing.T) {
	history := []Turn{
		{
			Index:  0,
			Status: TurnStatusCompleted,
			Items: []Item{
				{Kind: ItemKindUserMessage, Text: "查 AI 工具"},
				{Kind: ItemKindReasoning, Text: "我要先搜索"},
				{Kind: ItemKindToolCall, ToolCallID: "c1", ToolName: "web_search", Arguments: `{"q":"ai"}`},
				{Kind: ItemKindToolResult, ToolCallID: "c1", Output: `{"r":[...]}`},
				{Kind: ItemKindReasoning, Text: "结果不错,总结"},
				{Kind: ItemKindFinalAnswer, Text: "找到 5 个工具"},
			},
		},
	}
	msgs, err := HistoryToMessages(history, UserMessage{Text: "再查一个"})
	if err != nil {
		t.Fatalf("HistoryToMessages: %v", err)
	}

	// 预期 5 条:
	//   [0] user="查 AI 工具"
	//   [1] assistant reasoning="我要先搜索" tool_calls=[c1]
	//   [2] tool callID=c1 content="{r:[...]}"
	//   [3] assistant reasoning="结果不错,总结" content="找到 5 个工具"
	//   [4] user="再查一个"
	if len(msgs) != 5 {
		t.Fatalf("len=%d, want 5; msgs=%+v", len(msgs), msgs)
	}
	if msgs[0].Role != RoleUser || msgs[0].Content != "查 AI 工具" {
		t.Errorf("msg[0]: %+v", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || msgs[1].ReasoningContent != "我要先搜索" || len(msgs[1].ToolCalls) != 1 {
		t.Errorf("msg[1]: %+v", msgs[1])
	}
	if msgs[1].ToolCalls[0].ID != "c1" || msgs[1].ToolCalls[0].Name != "web_search" {
		t.Errorf("msg[1].ToolCalls[0]: %+v", msgs[1].ToolCalls[0])
	}
	if msgs[2].Role != RoleTool || msgs[2].ToolCallID != "c1" {
		t.Errorf("msg[2]: %+v", msgs[2])
	}
	if msgs[3].Role != RoleAssistant || msgs[3].ReasoningContent != "结果不错,总结" || msgs[3].Content != "找到 5 个工具" {
		t.Errorf("msg[3]: %+v", msgs[3])
	}
	if msgs[4].Role != RoleUser || msgs[4].Content != "再查一个" {
		t.Errorf("msg[4]: %+v", msgs[4])
	}
}

func TestHistoryToMessages_StepNesting(t *testing.T) {
	history := []Turn{
		{
			Status: TurnStatusCompleted,
			Items: []Item{
				{Kind: ItemKindUserMessage, Text: "Q"},
				{Kind: ItemKindStep, Label: "调研", Children: []Item{
					{Kind: ItemKindReasoning, Text: "R1"},
					{Kind: ItemKindStep, Label: "round 1", Children: []Item{
						{Kind: ItemKindToolCall, ToolCallID: "c1", ToolName: "t", Arguments: "{}"},
						{Kind: ItemKindToolResult, ToolCallID: "c1", Output: "out"},
					}},
				}},
				{Kind: ItemKindFinalAnswer, Text: "A"},
			},
		},
	}
	msgs, err := HistoryToMessages(history, UserMessage{Text: "Q2"})
	if err != nil {
		t.Fatalf("HistoryToMessages: %v", err)
	}
	// 预期:
	//   [0] user="Q"
	//   [1] assistant reasoning="R1" tool_calls=[c1]
	//   [2] tool callID=c1
	//   [3] assistant content="A"
	//   [4] user="Q2"
	if len(msgs) != 5 {
		t.Fatalf("len=%d, want 5; msgs=%+v", len(msgs), msgs)
	}
	if msgs[1].ReasoningContent != "R1" || len(msgs[1].ToolCalls) != 1 {
		t.Errorf("msg[1]: %+v", msgs[1])
	}
	if msgs[3].Content != "A" || msgs[3].ReasoningContent != "" {
		t.Errorf("msg[3]: %+v", msgs[3])
	}
}

func TestHistoryToMessages_OnlyInput(t *testing.T) {
	msgs, err := HistoryToMessages(nil, UserMessage{Text: "hi"})
	if err != nil {
		t.Fatalf("HistoryToMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != RoleUser || msgs[0].Content != "hi" {
		t.Errorf("got %+v", msgs)
	}
}

func TestHistoryToMessages_RejectsNonCompletedHistory(t *testing.T) {
	cases := []TurnStatus{
		TurnStatusQueued,
		TurnStatusInProgress,
		TurnStatusCancelled,
		TurnStatusFailed,
		"",
	}
	for _, status := range cases {
		_, err := HistoryToMessages([]Turn{
			{
				Index:  1,
				Status: status,
				Items: []Item{
					{Kind: ItemKindUserMessage, Text: "Q"},
				},
			},
		}, UserMessage{Text: "Q2"})
		if err == nil {
			t.Fatalf("status=%q: want error", status)
		}
	}
}
