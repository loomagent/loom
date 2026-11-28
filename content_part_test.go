package loom

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TextOf is the plain-text view of a message: its text parts in order, and nothing else.
func TestTextOf(t *testing.T) {
	tests := []struct {
		name  string
		parts []ContentPart
		want  string
	}{
		{name: "no parts", parts: nil, want: ""},
		{name: "one text part", parts: []ContentPart{TextPart("hello")}, want: "hello"},
		{name: "text parts in order, nothing between them", parts: []ContentPart{TextPart("first "), TextPart("second")}, want: "first second"},
		{
			name:  "an image is left out",
			parts: []ContentPart{TextPart("what is in "), ImagePart("https://example.com/cat.png", "low"), TextPart("this image?")},
			want:  "what is in this image?",
		},
		{name: "only an image", parts: []ContentPart{ImagePart("https://example.com/cat.png", "")}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TextOf(tt.parts); got != tt.want {
				t.Fatalf("TextOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A builder sets exactly the field its kind names, and an image with no detail chosen carries
// none, which is what tells the provider to keep its own default.
func TestContentPartBuilders(t *testing.T) {
	if got := TextPart("hello"); got != (ContentPart{Type: ContentText, Text: "hello"}) {
		t.Fatalf("TextPart() = %+v", got)
	}
	const url = "https://example.com/cat.png"
	if got := ImagePart(url, "low"); got != (ContentPart{Type: ContentImage, ImageURL: url, ImageDetail: "low"}) {
		t.Fatalf("ImagePart() = %+v", got)
	}
	if got := ImagePart(url, ""); got != (ContentPart{Type: ContentImage, ImageURL: url}) {
		t.Fatalf("ImagePart() without a detail = %+v", got)
	}
}

func TestTextUserMessage(t *testing.T) {
	got := TextUserMessage("hello")
	if !slices.Equal(got.Parts, []ContentPart{TextPart("hello")}) {
		t.Fatalf("Parts = %#v, want the text as its one part", got.Parts)
	}
	if got.Source != MessageSourceUnknown || got.Purpose != "" {
		t.Fatalf("a text message claims provenance it was not given: %+v", got)
	}
}

// This round's input reaches the model as it was given, images included, and keeps the
// provenance of every task input.
func TestHistoryToMessagesCarriesInputParts(t *testing.T) {
	parts := []ContentPart{TextPart("what is in this image?"), ImagePart("https://example.com/cat.png", "low")}
	msgs, err := HistoryToMessages(nil, UserMessage{Parts: parts, Source: MessageSourceTerminalUser})
	if err != nil {
		t.Fatalf("HistoryToMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len=%d, want 1; msgs=%+v", len(msgs), msgs)
	}
	got := msgs[0]
	if got.Role != RoleUser || !slices.Equal(got.Parts, parts) {
		t.Fatalf("input message = %+v, want its parts unchanged", got)
	}
	if got.Source() != MessageSourceTerminalUser || !got.IsExternalUserMessage() {
		t.Fatalf("input message lost its provenance: %+v", got)
	}
}

// A part of a kind no provider can send is refused, and never left out of the message.
func TestValidateChatRequestRejectsUnknownContentPartType(t *testing.T) {
	err := ValidateChatRequest(ChatRequest{Messages: []Message{
		{Role: RoleUser, Parts: []ContentPart{TextPart("task")}},
		{Role: RoleUser, Parts: []ContentPart{TextPart("and this: "), {Type: ContentType("file"), ImageURL: "x"}}},
	}})
	if err == nil || !strings.Contains(err.Error(), `message 1 uses unknown content part type "file"`) {
		t.Fatalf("error = %v, want the message and the part type named", err)
	}
	// It is not the missing-user failure, which callers match on.
	if errors.Is(err, ErrMissingUserMessage) {
		t.Fatalf("an unknown part type was reported as a missing user message: %v", err)
	}
	if err := ValidateChatRequest(ChatRequest{Messages: []Message{{Role: RoleUser, Parts: []ContentPart{
		TextPart("task"), ImagePart("https://example.com/cat.png", ""),
	}}}}); err != nil {
		t.Fatalf("text and image are known part types: %v", err)
	}
}
