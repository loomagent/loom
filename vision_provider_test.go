package loom_test

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/ark"
	"github.com/loomagent/loom/providers/deepseek"
	"github.com/loomagent/loom/providers/openrouter"
	"github.com/loomagent/loom/providers/zhipuai"
)

// wireMessage is what a provider put in one message of a request: the content is kept as it
// arrived, so the test compares the JSON the endpoint reads rather than a decoded guess.
type wireMessage struct {
	Role    string         `json:"role"`
	Content jsontext.Value `json:"content"`
}

// imageProviders are the built-in chat providers, built around a test server.
var imageProviders = []string{"ark", "deepseek", "openrouter", "zhipuai"}

func newImageModel(t *testing.T, provider, baseURL string, client *http.Client, vision bool) loom.ChatModel {
	t.Helper()
	caps := loom.ModelCapabilities{Vision: vision}
	var (
		model loom.ChatModel
		err   error
	)
	switch provider {
	case "ark":
		model, err = ark.New(ark.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: baseURL, HTTPClient: client, Capabilities: &caps})
	case "deepseek":
		model, err = deepseek.New(deepseek.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: baseURL, HTTPClient: client, Capabilities: &caps})
	case "openrouter":
		model, err = openrouter.New(openrouter.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: baseURL, HTTPClient: client, Capabilities: &caps})
	case "zhipuai":
		model, err = zhipuai.New(zhipuai.Config{APIKey: "fake-only", ModelName: "fake-model", BaseURL: baseURL, HTTPClient: client, Capabilities: &caps})
	default:
		t.Fatalf("unknown provider %q", provider)
	}
	if err != nil {
		t.Fatal(err)
	}
	return model
}

// Image input reads the same through every provider. An image on a user message reaches the
// endpoint as the content-parts array, a text-only message stays the plain string it always
// was, and a request that cannot carry its image is refused before anything is sent.
func TestImageInputAcrossProviders(t *testing.T) {
	const cat = "https://example.com/cat.png"
	image := loom.ImagePart(cat, "")
	imageWithDetail := loom.ImagePart(cat, "high")
	asking := []loom.Message{
		{Role: loom.RoleSystem, Parts: []loom.ContentPart{loom.TextPart("rules")}},
		{Role: loom.RoleUser, Parts: []loom.ContentPart{loom.TextPart("What is in this image?"), image, imageWithDetail}},
	}
	// What the endpoint reads for the second message: one part per message part, in order,
	// and a detail only where the caller set one.
	const askingContent = `[{"type":"text","text":"What is in this image?"},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/cat.png","detail":"high"}}]`
	textOnly := []loom.Message{{Role: loom.RoleUser, Parts: []loom.ContentPart{loom.TextPart("hi")}}}

	for _, provider := range imageProviders {
		t.Run(provider, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Given an endpoint that records what each request asked.
				var reached atomic.Int32
				requests := make(chan []wireMessage, 8)
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reached.Add(1)
					var body struct {
						Messages []wireMessage `json:"messages"`
					}
					if err := jsonv2.UnmarshalRead(r.Body, &body); err != nil {
						t.Error(err)
					}
					requests <- body.Messages
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"model":"fake-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
				}))
				defer server.Close()

				// When a model that declared vision is asked about an image, and separately
				// sent text only,
				vision := newImageModel(t, provider, server.URL, server.Client(), true)
				answer, err := vision.Chat(t.Context(), loom.ChatRequest{Messages: asking, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}})
				if err != nil || answer == nil || answer.Content != "ok" {
					t.Fatalf("image request: answer=%+v error=%v", answer, err)
				}
				sent := <-requests
				if reached.Load() != 1 || len(sent) != 2 {
					t.Fatalf("requests=%d messages=%d", reached.Load(), len(sent))
				}
				assertWireContent(t, sent[0], "system", `"rules"`)
				assertWireContent(t, sent[1], "user", askingContent)

				answer, err = vision.Chat(t.Context(), loom.ChatRequest{Messages: textOnly, Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled}})
				if err != nil || answer == nil || answer.Content != "ok" {
					t.Fatalf("text request: answer=%+v error=%v", answer, err)
				}
				sent = <-requests
				assertWireContent(t, sent[0], "user", `"hi"`)
			})
		})
	}
}

// The two requests no provider can take, an image for a model that declared no vision and an
// image on a message that is not the user's, fail locally on every entry point and never
// reach the endpoint.
func TestImageInputRefusedBeforeSendingAcrossProviders(t *testing.T) {
	image := loom.ImagePart("https://example.com/cat.png", "low")
	cases := []struct {
		name    string
		vision  bool
		message loom.Message
		wantErr string
	}{
		{
			name:    "a model that declared no vision",
			vision:  false,
			message: loom.Message{Role: loom.RoleUser, Parts: []loom.ContentPart{loom.TextPart("what is this?"), image}},
			wantErr: "does not declare vision support",
		},
		{
			name:    "an assistant message",
			vision:  true,
			message: loom.Message{Role: loom.RoleAssistant, Parts: []loom.ContentPart{loom.TextPart("look at this"), image}},
			wantErr: `role "assistant"`,
		},
		{
			name:    "a system message",
			vision:  true,
			message: loom.Message{Role: loom.RoleSystem, Parts: []loom.ContentPart{image}},
			wantErr: `role "system"`,
		},
		{
			name:    "a tool message",
			vision:  true,
			message: loom.Message{Role: loom.RoleTool, ToolCallID: "call-1", Parts: []loom.ContentPart{image}},
			wantErr: `role "tool"`,
		},
	}
	for _, provider := range imageProviders {
		for _, tc := range cases {
			for _, entry := range []string{"chat", "stream"} {
				t.Run(provider+" "+entry+" refuses "+tc.name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						// Given an endpoint ledger: a refused request must never reach it.
						var reached atomic.Int32
						server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							reached.Add(1)
							w.WriteHeader(http.StatusInternalServerError)
						}))
						defer server.Close()
						model := newImageModel(t, provider, server.URL, server.Client(), tc.vision)
						req := loom.ChatRequest{
							Messages:  []loom.Message{{Role: loom.RoleUser, Parts: []loom.ContentPart{loom.TextPart("task")}}, tc.message},
							Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
						}

						// When a caller sends it, then it fails as a local request error and
						// nothing is sent.
						var err error
						switch entry {
						case "chat":
							var answer *loom.ChatResponse
							answer, err = model.Chat(t.Context(), req)
							if answer != nil {
								t.Fatalf("a refused request returned an answer: %+v", answer)
							}
						case "stream":
							var stream loom.Stream
							stream, err = model.Stream(t.Context(), req)
							if stream != nil {
								t.Fatalf("a refused request returned a stream: %+v", stream)
							}
						}
						var local *loom.RequestValidationError
						if err == nil || !errors.As(err, &local) || !strings.Contains(err.Error(), tc.wantErr) {
							t.Fatalf("error = %v, want a local refusal naming %q", err, tc.wantErr)
						}
						if reached.Load() != 0 {
							t.Fatalf("a refused request reached the endpoint %d times", reached.Load())
						}
					})
				})
			}
		}
	}
}

// assertWireContent compares one wire message's role and content with the given JSON.
func assertWireContent(t *testing.T, got wireMessage, role, wantContent string) {
	t.Helper()
	if got.Role != role {
		t.Fatalf("role = %q, want %q", got.Role, role)
	}
	var gotContent, want any
	if err := jsonv2.Unmarshal(got.Content, &gotContent); err != nil {
		t.Fatal(err)
	}
	if err := jsonv2.Unmarshal([]byte(wantContent), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotContent, want) {
		t.Fatalf("%s content = %s, want %s", role, got.Content, wantContent)
	}
}
