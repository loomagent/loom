package loom

import "testing"

func TestHistoryToMessages_ReasoningPairing(t *testing.T) {
	history := []Turn{
		{
			Index:  0,
			Status: TurnStatusCompleted,
			Items: []Item{
				{Kind: ItemKindUserMessage, Text: "find AI tools"},
				{Kind: ItemKindReasoning, Text: "search first"},
				{Kind: ItemKindToolCall, ToolCallID: "c1", ToolName: "web_search", Arguments: `{"q":"ai"}`},
				{Kind: ItemKindToolResult, ToolCallID: "c1", Output: `{"r":[...]}`},
				{Kind: ItemKindReasoning, Text: "good results, summarize"},
				{Kind: ItemKindFinalAnswer, Text: "found 5 tools"},
			},
		},
	}
	msgs, err := HistoryToMessages(history, UserMessage{Text: "find one more"})
	if err != nil {
		t.Fatalf("HistoryToMessages: %v", err)
	}

	// Expect five:
	//   [0] user=find AI tools
	//   [1] assistant reasoning=search first, tool_calls=[c1]
	//   [2] tool callID=c1 content="{r:[...]}"
	//   [3] assistant reasoning=good results summarize, content=found 5 tools
	//   [4] user=find one more
	if len(msgs) != 5 {
		t.Fatalf("len=%d, want 5; msgs=%+v", len(msgs), msgs)
	}
	if msgs[0].Role != RoleUser || msgs[0].Content != "find AI tools" {
		t.Errorf("msg[0]: %+v", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || msgs[1].ReasoningContent != "search first" || len(msgs[1].ToolCalls) != 1 {
		t.Errorf("msg[1]: %+v", msgs[1])
	}
	if msgs[1].ToolCalls[0].ID != "c1" || msgs[1].ToolCalls[0].Name != "web_search" {
		t.Errorf("msg[1].ToolCalls[0]: %+v", msgs[1].ToolCalls[0])
	}
	if msgs[2].Role != RoleTool || msgs[2].ToolCallID != "c1" {
		t.Errorf("msg[2]: %+v", msgs[2])
	}
	if msgs[3].Role != RoleAssistant || msgs[3].ReasoningContent != "good results, summarize" || msgs[3].Content != "found 5 tools" {
		t.Errorf("msg[3]: %+v", msgs[3])
	}
	if msgs[4].Role != RoleUser || msgs[4].Content != "find one more" {
		t.Errorf("msg[4]: %+v", msgs[4])
	}
}

func TestHistoryToMessages_StepNesting(t *testing.T) {
	history := []Turn{
		{
			Status: TurnStatusCompleted,
			Items: []Item{
				{Kind: ItemKindUserMessage, Text: "Q"},
				{Kind: ItemKindStep, Label: "research", Children: []Item{
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
	// Expect:
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
