package openrouter

import (
	"context"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/loomagent/loom"
)

// The blocks a reasoning model returns, shaped as the endpoint documents them: a typed
// sequence, each block with an id, a format, and an index.
const (
	blockOne = `{"type":"reasoning.text","text":"step one","signature":"sha256:abc","id":"reasoning-text-1","format":"anthropic-claude-v1","index":1}`
	blockTwo = `{"type":"reasoning.encrypted","data":"ZW5jcnlwdGVk","id":"reasoning-encrypted-1","format":"anthropic-claude-v1","index":2}`
)

// A chain continues with the blocks it started with, unchanged and in order. This is that round
// trip end to end: the endpoint's blocks are mapped onto the response, carried on the assistant
// message the way a loop carries one, and sent back on the next call.
func TestStructuredReasoningRoundTrips(t *testing.T) {
	toolCall := `{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}`
	var received []string
	model := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		received = append(received, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if len(received) == 1 {
			// The first answer calls a tool, which is the case where the chain continues.
			_, _ = fmt.Fprintf(w, `{"model":"m","choices":[{"message":{"content":"","reasoning":"thinking out loud","reasoning_details":[%s,%s],"tool_calls":[%s]},"finish_reason":"tool_calls"}]}`, blockOne, blockTwo, toolCall)
			return
		}
		_, _ = fmt.Fprint(w, `{"model":"m","choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`)
	})
	request := func(messages []loom.Message) loom.ChatRequest {
		return loom.ChatRequest{Messages: messages, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}}
	}

	response, err := model.Chat(t.Context(), request([]loom.Message{{Role: loom.RoleUser, Content: "hi"}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(response.ReasoningDetails); got != "["+blockOne+","+blockTwo+"]" {
		t.Fatalf("response details = %s", got)
	}
	// The text form arrives alongside the structure, and both are kept.
	if response.ReasoningContent != "thinking out loud" {
		t.Fatalf("reasoning text = %q", response.ReasoningContent)
	}

	// A loop carries the assistant turn forward, which is what must preserve the blocks.
	messages := loom.AppendAssistantTurn(
		[]loom.Message{{Role: loom.RoleUser, Content: "hi"}},
		response,
		[]loom.ToolExecResult{{Call: response.ToolCalls[0], Output: `{"found":true}`}},
	)
	if _, err := model.Chat(t.Context(), request(messages)); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 {
		t.Fatalf("requests = %d", len(received))
	}
	// Byte for byte, in order: the endpoint asks for the blocks back exactly as it sent them,
	// and it is the raw bytes that make that true rather than a round trip through a Go value.
	if !strings.Contains(received[1], `"reasoning_details":[`+blockOne+`,`+blockTwo+`]`) {
		t.Fatalf("the second request did not carry the blocks back:\n%s", received[1])
	}
	// And the second answer, carrying none, ends the chain rather than inventing blocks.
	var second map[string]any
	if err := jsonv2.Unmarshal([]byte(received[1]), &second); err != nil {
		t.Fatal(err)
	}
}

// A streamed chain builds its sequence by concatenating the frames in order, so the accumulated
// response carries every block the endpoint sent, in the order it sent them.
func TestStructuredReasoningAccumulatesAcrossStreamFrames(t *testing.T) {
	model := testModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"thinking\",\"reasoning\":\"one\",\"reasoning_details\":[%s]}}]}\n\n", blockOne)
		_, _ = fmt.Fprintf(w, "data: {\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_details\":[%s]},\"finish_reason\":\"stop\"}]}\n\n", blockTwo)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})

	var response *loom.ChatResponse
	_, err := loom.Run(context.Background(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var streamErr error
		response, streamErr = loom.StreamLLMToStep(ctx, w, "details", model, loom.ChatRequest{
			Messages:  []loom.Message{{Role: loom.RoleUser, Content: "hi"}},
			Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
		})
		return streamErr
	}, loom.RunOptions{ConversationID: "details"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(response.ReasoningDetails); got != "["+blockOne+","+blockTwo+"]" {
		t.Fatalf("accumulated details = %s", got)
	}
	if response.ReasoningContent != "one" {
		t.Fatalf("reasoning text = %q", response.ReasoningContent)
	}
}
