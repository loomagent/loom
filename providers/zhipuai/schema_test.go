package zhipuai

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/loomagent/loom"
)

func schemaRequest(t *testing.T) loom.ChatRequest {
	t.Helper()
	var schema jsonschema.Schema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"answer":{"$ref":"#/$defs/answer"},"tags":{"type":"array","items":{"type":"string","enum":["a","b"]},"minItems":1}},"required":["answer","tags"],"additionalProperties":false,"$defs":{"answer":{"type":"integer","minimum":3,"maximum":9}}}`), &schema); err != nil {
		t.Fatal(err)
	}
	req := request()
	req.StructuredOutput = &loom.StructuredOutput{Mode: loom.StructuredOutputJSONSchema, Name: "nested_answer", Description: "A constrained answer", Schema: &schema}
	return req
}

func TestJSONSchemaTransport(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, declared := range []loom.StructuredOutputMode{"", loom.StructuredOutputJSONSchema} {
			t.Run(fmt.Sprintf("stream=%t/declared=%s", stream, declared), func(t *testing.T) {
				req := schemaRequest(t)
				// Streaming tool setup reassigns provider extras; it must preserve
				// the schema and explicit reasoning controls as well.
				req.Tools = []*loom.ToolInfo{{Name: "lookup"}}
				wantBytes, err := json.Marshal(req.StructuredOutput.Schema)
				if err != nil {
					t.Fatal(err)
				}
				var wantSchema any
				if err := json.Unmarshal(wantBytes, &wantSchema); err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int32
				m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					body := readRequest(t, r)
					want := map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "nested_answer", "description": "A constrained answer", "strict": true, "schema": wantSchema}}
					if !reflect.DeepEqual(body["response_format"], want) {
						t.Errorf("response_format=%#v want=%#v", body["response_format"], want)
					}
					if body["reasoning_effort"] != "low" || !reflect.DeepEqual(body["thinking"], map[string]any{"type": "enabled"}) {
						t.Errorf("reasoning lost: %v", body)
					}
					if stream {
						if body["stream"] != true || body["tool_stream"] != true {
							t.Errorf("stream flags: %v", body)
						}
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"model\":\"glm-response-version\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{\\\"answer\\\":7,\"}}]}\n\n")
						fmt.Fprint(w, "data: {\"model\":\"glm-response-version\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\\\"tags\\\":[\\\"a\\\"]}\"},\"finish_reason\":\"stop\"}]}\n\n")
						fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"total_tokens\":12}}\n\ndata: [DONE]\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"model":"glm-response-version","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"answer\":7,\"tags\":[\"a\"]}"}}]}`)
					}
				})
				m.capabilities.StructuredOutput = declared
				var content string
				if stream {
					s, err := m.Stream(t.Context(), req)
					if err != nil {
						t.Fatal(err)
					}
					defer s.Close()
					var finish loom.FinishReason
					var tokens uint64
					for {
						chunk, err := s.Recv()
						if errors.Is(err, io.EOF) {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						content += chunk.ContentDelta
						if chunk.FinishReason != "" {
							finish = chunk.FinishReason
						}
						if chunk.Usage != nil {
							tokens = chunk.Usage.TotalTokens
						}
					}
					if finish != loom.FinishReasonStop || tokens != 12 {
						t.Fatalf("finish=%s tokens=%d", finish, tokens)
					}
				} else {
					response, err := m.Chat(t.Context(), req)
					if err != nil {
						t.Fatal(err)
					}
					content = response.Content
					if response.Model != "glm-response-version" || response.FinishReason != loom.FinishReasonStop {
						t.Fatalf("response=%+v", response)
					}
				}
				if content != `{"answer":7,"tags":["a"]}` || calls.Load() != 1 {
					t.Fatalf("content=%s calls=%d", content, calls.Load())
				}
			})
		}
	}
}

func TestJSONSchemaLocalValidationAndBusinessGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caps   loom.ModelCapabilities
		mutate func(*loom.ChatRequest)
	}{
		{name: "object declaration", caps: loom.ModelCapabilities{StructuredOutput: loom.StructuredOutputJSONObject}},
		{name: "none declaration", caps: loom.ModelCapabilities{StructuredOutput: loom.StructuredOutputNone}},
		{name: "missing schema", mutate: func(r *loom.ChatRequest) { r.StructuredOutput.Schema = nil }},
		{name: "unknown mode", mutate: func(r *loom.ChatRequest) { r.StructuredOutput.Mode = "invalid" }},
		{name: "capability-only mode", mutate: func(r *loom.ChatRequest) { r.StructuredOutput.Mode = loom.StructuredOutputNone }},
		{name: "unknown reasoning", mutate: func(r *loom.ChatRequest) { r.Reasoning.Mode = "invalid" }},
		{name: "missing business effort", caps: loom.ModelCapabilities{Reasoning: loom.ReasoningSupportToggleable, ReasoningEfforts: []loom.ReasoningEffort{"low"}}, mutate: func(r *loom.ChatRequest) { r.Reasoning.Effort = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			m := testModel(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
			m.capabilities = tc.caps
			req := schemaRequest(t)
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			for _, stream := range []bool{false, true} {
				var err error
				if stream {
					_, err = m.Stream(context.Background(), req)
				} else {
					_, err = m.Chat(context.Background(), req)
				}
				var local *loom.RequestValidationError
				if !errors.As(err, &local) {
					t.Fatalf("stream=%t expected typed local validation error, got %v", stream, err)
				}
			}
			if calls.Load() != 0 {
				t.Fatalf("invalid requests sent: %d", calls.Load())
			}
		})
	}
}

func TestJSONSchemaUpstreamRejectionIsNotLocal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				readRequest(t, r)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":{"code":"1210","message":"response_format json_schema is unsupported"}}`)
			})
			var err error
			if stream {
				_, err = m.Stream(t.Context(), schemaRequest(t))
			} else {
				_, err = m.Chat(t.Context(), schemaRequest(t))
			}
			var api *APIError
			var local *loom.RequestValidationError
			if !errors.As(err, &api) || errors.As(err, &local) || api.StatusCode != 400 || api.Code != "1210" || calls.Load() != 1 {
				t.Fatalf("calls=%d error=%v", calls.Load(), err)
			}
		})
	}
}
