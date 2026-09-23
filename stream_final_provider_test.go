package loom_test

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/deepseek"
	"github.com/loomagent/loom/react"
)

func TestUserFinalDeltasPassThroughRealProviderAdapter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given an in-memory HTTP endpoint using the provider's real SSE protocol.
		// The official SDK and production adapter decode these fixtures; no paid
		// endpoint or credentials are used, and JSON v2 validates the wire contract.
		frames := make(chan string)
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var req struct {
				Tools    []jsontext.Value `json:"tools"`
				Thinking struct {
					Type string `json:"type"`
				} `json:"thinking"`
				Effort string `json:"reasoning_effort"`
				Stream bool   `json:"stream"`
			}
			if err := jsonv2.Unmarshal(body, &req); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !req.Stream || req.Thinking.Type != "enabled" || req.Effort != "high" {
				t.Errorf("explicit reasoning changed: %s", body)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			write := func(frame string) {
				var value any
				if err := jsonv2.Unmarshal([]byte(frame), &value); err != nil {
					t.Error(err)
					return
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
				}
			}
			if len(req.Tools) > 0 {
				write(`{"id":"research","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"finish","type":"function","function":{"name":"finalize_answer","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			} else {
				for frame := range frames {
					write(frame)
				}
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		model, err := deepseek.New(deepseek.Config{APIKey: "test-only", ModelName: "contract-fixture", BaseURL: server.URL, HTTPClient: server.Client(), Capabilities: &loom.ModelCapabilities{Reasoning: loom.ReasoningSupportToggleable, ReasoningEfforts: []loom.ReasoningEffort{loom.ReasoningEffortHigh}}})
		if err != nil {
			t.Fatal(err)
		}
		terminal := loom.NewArgsTool(loom.MustArgsContract("finalize_answer"), "End the tool phase", func(context.Context, loom.Args) (string, error) { return `{"ok":true}`, nil }, loom.WithEndsToolPhase())
		sink := loom.NewMemorySink()
		done := make(chan struct{})
		var turn *loom.Turn
		var runErr error
		go func() {
			defer close(done)
			turn, runErr = loom.Run(t.Context(), func(ctx context.Context, w loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
				_, err := react.RunToFinalAnswer(ctx, w, react.Config{Model: model, Tools: loom.NewToolRegistry(terminal), Reasoning: loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: loom.ReasoningEffortHigh}})
				return err
			}, loom.RunOptions{ConversationID: "wire-stream", Sinks: []loom.Sink{sink}, StrictSink: true})
		}()
		// When wire frames are released independently, the sink sees each delta
		// while the HTTP stream is still open, through the real provider adapter.
		for i, delta := range []string{`{"reasoning_content":"reason"}`, `{"reasoning_content":" more"}`, `{"content":"answer"}`, `{"content":" now"}`} {
			frame := fmt.Sprintf(`{"id":"final","object":"chat.completion.chunk","model":"contract-fixture","choices":[{"index":0,"delta":%s,"finish_reason":null}]}`, delta)
			select {
			case frames <- frame:
			case <-done:
				t.Fatalf("early stop: %v", runErr)
			}
			synctest.Wait()
			if len(sink.DeltaEvents()) != i+1 {
				t.Fatalf("provider delta %d was buffered: %v", i+1, sink.DeltaEvents())
			}
		}
		frames <- `{"id":"final","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":4,"total_tokens":6}}`
		close(frames)
		<-done
		// Then the answer commits once, after both channels have been delivered.
		if runErr != nil || turn.Status != loom.TurnStatusCompleted || turn.Usage.TotalTokens != 6 {
			t.Fatalf("turn=%+v err=%v", turn, runErr)
		}
	})
}
