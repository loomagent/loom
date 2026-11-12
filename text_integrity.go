// Text integrity diagnostics are observational: they never reject, repair, or retry output.
package loom

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// TextIntegrityObservation is an immutable diagnostic snapshot.
// Sample contains at most 64 runes near the suspect text.
// A finding is a suspicion (a model may intentionally quote a replacement character).
type TextIntegrityObservation struct {
	Invocation     Invocation
	Source         string // model, runtime_item, or tool_result
	Field          string
	Kind           string // invalid_utf8, replacement_character, cp1252_mojibake
	ByteOffset     int
	Sample         string
	Partial        bool
	ReportedModel  string
	ToolCallID     string
	ToolName       string
	ConversationID string
	ItemPath       string
}

// TextIntegrityObserver must return promptly and be safe for concurrent calls.
// It has no return value or access to mutable responses. Panics are contained.
type TextIntegrityObserver func(context.Context, TextIntegrityObservation)
type textIntegrityObserverHolder struct{ observer TextIntegrityObserver }
type textIntegrityObserverKey struct{}
type textIntegrityCaptureKey struct{}

var defaultTextIntegrityObserver atomic.Pointer[textIntegrityObserverHolder]

// SetTextIntegrityObserver installs the process-wide diagnostic observer; nil disables it.
// Applications normally set this once at startup. Loom owns no logging/webhook service.
func SetTextIntegrityObserver(observer TextIntegrityObserver) {
	defaultTextIntegrityObserver.Store(&textIntegrityObserverHolder{observer: observer})
}

// WithTextIntegrityObserver overrides the process observer for ctx, including explicit nil.
func WithTextIntegrityObserver(ctx context.Context, observer TextIntegrityObserver) context.Context {
	return context.WithValue(ctx, textIntegrityObserverKey{}, textIntegrityObserverHolder{observer: observer})
}

func textIntegrityObserverFrom(ctx context.Context) TextIntegrityObserver {
	if holder, ok := ctx.Value(textIntegrityObserverKey{}).(textIntegrityObserverHolder); ok {
		return holder.observer
	}
	if holder := defaultTextIntegrityObserver.Load(); holder != nil {
		return holder.observer
	}
	return nil
}

func beginTextIntegrity(ctx context.Context) (context.Context, *textIntegrityCapture) {
	observer := textIntegrityObserverFrom(ctx)
	if observer == nil {
		return ctx, nil
	}
	invocation := InvocationFromContext(ctx)
	if previous, _ := ctx.Value(textIntegrityCaptureKey{}).(*textIntegrityCapture); previous != nil && previous.invocation.InvocationID == invocation.InvocationID {
		return ctx, nil
	}
	capture := &textIntegrityCapture{ctx: ctx, observer: observer, invocation: invocation, tools: make(map[int]*integrityTool)}
	return context.WithValue(ctx, textIntegrityCaptureKey{}, capture), capture
}

type integrityTool struct {
	id, name  string
	arguments strings.Builder
}
type textIntegrityCapture struct {
	ctx                context.Context
	observer           TextIntegrityObserver
	invocation         Invocation
	mu                 sync.Mutex
	done               bool
	content, reasoning strings.Builder
	details            []string
	tools              map[int]*integrityTool
	reportedModel      string
}

func (c *textIntegrityCapture) add(chunk *Chunk) {
	if chunk == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return
	}
	c.content.WriteString(chunk.ContentDelta)
	c.reasoning.WriteString(chunk.ReasoningContentDelta)
	if len(chunk.ReasoningDetails) > 0 {
		c.details = append(c.details, string(chunk.ReasoningDetails))
	}
	if chunk.Model != "" {
		c.reportedModel = chunk.Model
	}
	for _, delta := range chunk.ToolCallDeltas {
		tool := c.tools[delta.Index]
		if tool == nil {
			tool = &integrityTool{}
			c.tools[delta.Index] = tool
		}
		if delta.ID != "" {
			tool.id = delta.ID
		}
		if delta.Name != "" {
			tool.name = delta.Name
		}
		tool.arguments.WriteString(delta.Arguments)
	}
}

func (c *textIntegrityCapture) response(response *ChatResponse, partial bool) {
	if response == nil {
		return
	}
	base := TextIntegrityObservation{Invocation: c.invocation, Source: "model", Partial: partial, ReportedModel: response.Model}
	report := integrityReporter(c.ctx, c.observer, base)
	inspectIntegrityValue(response.Content, "content", partial, report)
	inspectIntegrityValue(response.ReasoningContent, "reasoning_content", partial, report)
	inspectIntegrityValue(string(response.ReasoningDetails), "reasoning_details", partial, report)
	for index, tool := range response.ToolCalls {
		toolBase := base
		toolBase.ToolCallID = tool.ID
		toolBase.ToolName = tool.Name
		toolReport := integrityReporter(c.ctx, c.observer, toolBase)
		prefix := fmt.Sprintf("tool_calls[%d]", index)
		inspectIntegrityValue(tool.ID, prefix+".id", partial, toolReport)
		inspectIntegrityValue(tool.Name, prefix+".name", partial, toolReport)
		inspectIntegrityValue(tool.Arguments, prefix+".arguments", partial, toolReport)
	}
}

func (c *textIntegrityCapture) finish(partial bool) {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	response := ChatResponse{Content: c.content.String(), ReasoningContent: c.reasoning.String(), Model: c.reportedModel}
	details := c.details
	c.details = nil
	indices := make([]int, 0, len(c.tools))
	for i := range c.tools {
		indices = append(indices, i)
	}
	slices.Sort(indices)
	for _, i := range indices {
		t := c.tools[i]
		response.ToolCalls = append(response.ToolCalls, ToolCall{ID: t.id, Name: t.name, Arguments: t.arguments.String()})
	}
	c.mu.Unlock()
	c.response(&response, partial)
	report := integrityReporter(c.ctx, c.observer, TextIntegrityObservation{Invocation: c.invocation, Source: "model", Partial: partial, ReportedModel: response.Model})
	for _, raw := range details {
		inspectIntegrityValue(raw, "reasoning_details", partial, report)
	}
}

type textIntegrityStream struct {
	Stream
	capture *textIntegrityCapture
}

func (s *textIntegrityStream) Recv() (*Chunk, error) {
	chunk, err := s.Stream.Recv()
	s.capture.add(chunk)
	if err != nil {
		s.capture.finish(!errors.Is(err, io.EOF))
	}
	return chunk, err
}
func (s *textIntegrityStream) Close() error {
	err := s.Stream.Close()
	s.capture.finish(true)
	return err
}

// The finished-item boundary also covers host-produced text and custom tool schedulers.
func observeItemIntegrity(ctx context.Context, conversationID string, item Item) {
	observer := textIntegrityObserverFrom(ctx)
	if observer == nil {
		return
	}
	base := TextIntegrityObservation{Invocation: InvocationFromContext(ctx), Source: "runtime_item", ToolCallID: item.ToolCallID, ToolName: item.ToolName, ConversationID: conversationID, ItemPath: item.Path, Partial: item.Status != ItemStatusCompleted}
	if item.Kind == ItemKindToolResult {
		base.Source = "tool_result"
	}
	report := integrityReporter(ctx, observer, base)
	inspectIntegrityValue(item.Text, "text", base.Partial, report)
	inspectIntegrityValue(item.Label, "label", false, report)
	inspectIntegrityValue(item.Arguments, "arguments", false, report)
	inspectIntegrityValue(item.Output, "output", false, report)
	inspectIntegrityValue(item.ToolName, "tool_name", false, report)
	inspectIntegrityValue(item.ToolCallID, "tool_call_id", false, report)
	if item.Error != nil {
		inspectIntegrityValue(item.Error.Message, "error.message", false, report)
		inspectIntegrityValue(item.Error.Code, "error.code", false, report)
	}
}

func integrityReporter(ctx context.Context, observer TextIntegrityObserver, base TextIntegrityObservation) func(string, string, int, string) {
	seen := make(map[string]bool)
	return func(field, kind string, offset int, sample string) {
		key := field + "\x00" + kind
		if seen[key] {
			return
		}
		seen[key] = true
		event := base
		event.Field = field
		event.Kind = kind
		event.ByteOffset = offset
		event.Sample = sample
		func() {
			defer func() {
				if recover() != nil {
					slog.WarnContext(ctx, "loom text integrity observer panicked")
				}
			}()
			observer(ctx, event)
		}()
	}
}

// Decode valid JSON before inspection so escaped text such as "\\ufffd" is observed too.
// Invalid/partial JSON is inspected as raw text, without returning a validation error.
func inspectIntegrityValue(value, field string, partial bool, report func(string, string, int, string)) {
	if value == "" {
		return
	}
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) == nil {
		var walk func(any, string)
		walk = func(v any, path string) {
			switch x := v.(type) {
			case string:
				inspectIntegrityText(x, path, partial, report)
			case map[string]any:
				keys := make([]string, 0, len(x))
				for key := range x {
					keys = append(keys, key)
				}
				slices.Sort(keys)
				for _, key := range keys {
					inspectIntegrityText(key, path+".$key", partial, report)
					walk(x[key], path+"."+key)
				}
			case []any:
				for i, v := range x {
					walk(v, fmt.Sprintf("%s[%d]", path, i))
				}
			}
		}
		walk(decoded, field)
		return
	}
	inspectIntegrityText(value, field, partial, report)
}

func inspectIntegrityText(text, field string, partial bool, report func(string, string, int, string)) {
	pairs := 0
	previousLead := false
	firstPair := -1
	previousOffset := 0
	for offset := 0; offset < len(text); {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if r == utf8.RuneError {
			if size == 1 {
				if partial && !utf8.FullRuneInString(text[offset:]) {
					break
				}
				report(field, "invalid_utf8", offset, integritySample(text, offset))
			} else {
				report(field, "replacement_character", offset, integritySample(text, offset))
			}
		}
		if previousLead && integrityCPTail(r) {
			pairs++
			if firstPair < 0 {
				firstPair = previousOffset
			}
			if pairs == 2 {
				report(field, "cp1252_mojibake", firstPair, integritySample(text, firstPair))
			}
			previousLead = false
		} else {
			previousLead = r >= 0xC0 && r <= 0xFF
			previousOffset = offset
		}
		offset += size
	}
}

func integrityCPTail(r rune) bool {
	if r >= 0x80 && r <= 0xBF || r == utf8.RuneError {
		return true
	}
	return strings.ContainsRune("€‚ƒ„…†‡ˆ‰Š‹ŒŽ‘’“”•–—˜™š›œžŸ", r)
}

func integritySample(text string, offset int) string {
	// Start up to 16 runes before the finding and include at most 64 runes total.
	start := offset
	for i := 0; i < 16 && start > 0; i++ {
		_, size := utf8.DecodeLastRuneInString(text[:start])
		start -= size
	}
	var sample strings.Builder
	count := 0
	for _, r := range text[start:] {
		if count == 64 {
			break
		}
		sample.WriteRune(r)
		count++
	}
	return sample.String()
}
