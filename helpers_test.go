package loom

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"testing"
	"time"
)

// AppendAssistantTurn is what a ReAct loop calls between rounds, so what it puts in front of
// the model next matters: the turn it just had, and one message per tool result.
func TestAppendAssistantTurnPairsResultsWithMessages(t *testing.T) {
	response := &ChatResponse{
		Content:          "calling",
		ReasoningContent: "why",
		ToolCalls:        []ToolCall{{ID: "c1", Name: "search", Arguments: `{}`}},
	}
	results := []ToolExecResult{
		{Call: ToolCall{ID: "c1", Name: "search"}, Output: `{"found":true}`},
		{Call: ToolCall{ID: "c2", Name: "lookup"}, Err: errors.New("no such tool")},
	}
	msgs := AppendAssistantTurn([]Message{{Role: RoleUser, Content: "hi"}}, response, results)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d", len(msgs))
	}
	assistant := msgs[1]
	if assistant.Role != RoleAssistant || assistant.Content != "calling" ||
		assistant.ReasoningContent != "why" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant = %+v", assistant)
	}
	for index, want := range []struct{ id, content string }{
		{"c1", `{"found":true}`},
		// A failed tool is reported as a message rather than silence, so the model can
		// decide whether to try something else.
		{"c2", "tool execution error: no such tool"},
	} {
		tool := msgs[index+2]
		if tool.Role != RoleTool || tool.ToolCallID != want.id || tool.Content != want.content {
			t.Errorf("tool message = %+v", tool)
		}
	}
	// The caller's slice is not written through.
	if msgs[0].Content != "hi" {
		t.Fatalf("first message = %+v", msgs[0])
	}
}

// A streamed item keeps the chunks a Sink saw, and SetFinalText replaces the text the item
// ends with when the last frame differs from the pieces.
func TestMemorySinkCollectsDeltasAndStreamFinalTextWins(t *testing.T) {
	sink := NewMemorySink()
	_, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		err := w.StreamReasoning(ctx, "Thinking", func(stream ReasoningStream) error {
			if err := stream.AppendText(ctx, "half "); err != nil {
				return err
			}
			if err := stream.AppendText(ctx, "and half"); err != nil {
				return err
			}
			stream.SetFinalText("the whole thing")
			return nil
		})
		if err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "done")
	}, RunOptions{ConversationID: "sink", Sinks: []Sink{sink}})
	if err != nil {
		t.Fatal(err)
	}

	deltas := sink.DeltaEvents()
	if len(deltas) != 2 || deltas[0].Chunk != "half " || deltas[1].Chunk != "and half" {
		t.Fatalf("deltas = %+v", deltas)
	}
	if deltas[0].Channel != DeltaChannelText || deltas[0].ItemPath == "" {
		t.Fatalf("delta = %+v", deltas[0])
	}
	var reasoning string
	for _, ev := range sink.FinishedEvents() {
		if ev.Item.Kind == ItemKindReasoning {
			reasoning = ev.Item.Text
		}
	}
	if reasoning != "the whole thing" {
		t.Fatalf("the item ended with %q, want the SetFinalText value", reasoning)
	}

	// Reset is how one sink serves several turns.
	sink.Reset()
	if len(sink.StartedEvents()) != 0 || len(sink.DeltaEvents()) != 0 ||
		len(sink.FinishedEvents()) != 0 || len(sink.LLMCalls()) != 0 {
		t.Fatal("Reset left events behind")
	}
}

// A writer's path is what a helper or a log line reports, so it is there from the root down.
func TestWriterPathReachesTheItem(t *testing.T) {
	var paths []string
	_, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		paths = append(paths, w.Path())
		if err := w.Step(ctx, "Stage", func(ctx context.Context, step Step) error {
			paths = append(paths, step.Path())
			return nil
		}); err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "done")
	}, RunOptions{ConversationID: "paths"})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "turn[0]" || paths[1] != "turn[0].step[0]" {
		t.Fatalf("paths = %v", paths)
	}
}

// The Turn returned when the arguments themselves are wrong is still a Turn, which is what
// lets a caller read its CloseReason instead of guarding against nil.
func TestRunReturnsATurnWhenItsArgumentsAreWrong(t *testing.T) {
	turn, err := Run(context.Background(), func(context.Context, TurnWriter, []Turn, UserMessage) error {
		t.Error("a handler must not run")
		return nil
	}, RunOptions{})
	if err == nil {
		t.Fatal("a missing ConversationID must fail")
	}
	if turn == nil || turn.Status != TurnStatusFailed || turn.CloseReason == nil || turn.CloseReason.Code != CloseCodeAgentError {
		t.Fatalf("turn = %+v", turn)
	}
	if turn.CloseReason.Message == "" || turn.ConversationID != "" {
		t.Fatalf("turn = %+v", turn)
	}
}

// A cancelled turn says who cancelled it: a host shutting down and a control plane asking are
// not the same event as a user stopping one turn.
func TestRunNamesTheCanceller(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want CloseCode
	}{
		{name: "user", err: errors.New("stopped"), want: CloseCodeUserCancel},
		{name: "host", err: ErrHostShutdown, want: CloseCodeHostShutdown},
		{name: "control plane", err: ErrExternalCancel, want: CloseCodeExternalCancel},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(testCase.err)
			turn, err := Run(ctx, func(ctx context.Context, _ TurnWriter, _ []Turn, _ UserMessage) error {
				return ctx.Err()
			}, RunOptions{ConversationID: "cancel"})
			if err != nil {
				t.Fatal(err)
			}
			if turn.Status != TurnStatusCancelled || turn.CloseReason.Code != testCase.want {
				t.Fatalf("turn = %+v (%+v)", turn.Status, turn.CloseReason)
			}
		})
	}
}

// The error a caller gets back from a model call says what was classified and how many
// attempts it took, and stays unwrappable so an application can still test for its own error.
func TestClassifiedErrorCarriesItsClassAndCause(t *testing.T) {
	cause := errors.New("upstream said no")
	classified := &ClassifiedError{Class: ErrorClassRateLimit, Attempts: 3, Err: cause}
	message := classified.Error()
	if !strings.Contains(message, "rate_limit") || !strings.Contains(message, "attempts=3") {
		t.Fatalf("message = %q", message)
	}
	if !errors.Is(classified, cause) {
		t.Fatal("the cause is not reachable")
	}
	class, ok := ErrorClassOf(classified)
	if !ok || class != ErrorClassRateLimit {
		t.Fatalf("ErrorClassOf = %q %t", class, ok)
	}
	// A wrapped one is found too, and an ordinary error reports no class rather than a guess.
	wrapped := errors.Join(errors.New("context"), classified)
	if class, ok := ErrorClassOf(wrapped); !ok || class != ErrorClassRateLimit {
		t.Fatalf("wrapped ErrorClassOf = %q %t", class, ok)
	}
	if class, ok := ErrorClassOf(cause); ok || class != ErrorClassUnknown {
		t.Fatalf("unclassified = %q %t", class, ok)
	}
	// A nil receiver still renders, because it can reach a log through %v.
	if message := (*ClassifiedError)(nil).Error(); message == "" {
		t.Fatal("a nil ClassifiedError rendered nothing")
	}
}

func TestErrorClassNames(t *testing.T) {
	for class, want := range map[ErrorClass]string{
		ErrorClassUnknown:   "unknown",
		ErrorClassTransient: "transient",
		ErrorClassRateLimit: "rate_limit",
		ErrorClassPermanent: "permanent",
		ErrorClass(99):      "error_class_99",
	} {
		if got := class.String(); got != want {
			t.Errorf("ErrorClass(%q).String() = %q, want %q", class, got, want)
		}
	}
}

// A local rejection is marked as one, because a probe must not read it as the server refusing
// a capability.
func TestLocalRequestErrorIsRecognizable(t *testing.T) {
	if LocalRequestError(nil) != nil {
		t.Fatal("no error, no annotation")
	}
	cause := errors.New("unknown reasoning send")
	annotated := LocalRequestError(cause)
	if !errors.Is(annotated, cause) {
		t.Fatal("the cause is not reachable")
	}
	var local *RequestValidationError
	if !errors.As(annotated, &local) || !strings.Contains(local.Error(), "local request validation") {
		t.Fatalf("annotated = %v", annotated)
	}
}

// A model name is split at its first boundary, because the model part may contain slashes of
// its own.
func TestSplitModelName(t *testing.T) {
	for name, want := range map[string][2]string{
		"deepseek/deepseek-chat":  {"deepseek", "deepseek-chat"},
		"openrouter/openai/gpt-5": {"openrouter", "openai/gpt-5"},
		"unnamespaced":            {"unnamespaced", ""},
		"":                        {"", ""},
	} {
		provider, model := SplitModelName(name)
		if provider != want[0] || model != want[1] {
			t.Errorf("SplitModelName(%q) = %q, %q", name, provider, model)
		}
	}
}

// A probe model is isolated from business configuration, and whether it omits the parameters
// is what distinguishes observing a default from sending an explicit request.
func TestReasoningProbeCapabilities(t *testing.T) {
	explicit := ReasoningProbeCapabilities(false)
	if !explicit.reasoningProbe || explicit.Reasoning != ReasoningSupport("") {
		t.Fatalf("explicit = %+v", explicit)
	}
	omitted := ReasoningProbeCapabilities(true)
	if !omitted.reasoningProbe || omitted.Reasoning != ReasoningSupportNone {
		t.Fatalf("omitted = %+v", omitted)
	}
}

// ConstJSON is raw JSON, so a literal null is a const and a large integer keeps its digits.
func TestConstJSON(t *testing.T) {
	if got := ConstJSON(nil); string(got) != "null" {
		t.Fatalf("ConstJSON(nil) = %s", got)
	}
	if got := ConstJSON(9007199254740993); string(got) != "9007199254740993" {
		t.Fatalf("large const = %s", got)
	}
	if got := ConstJSON("text"); string(got) != `"text"` {
		t.Fatalf("string const = %s", got)
	}
	// A value that cannot be marshaled is a mistake in the schema being built, so it panics
	// where the mistake is rather than producing a const that means something else.
	defer func() {
		if recover() == nil {
			t.Fatal("an unmarshalable const must panic")
		}
	}()
	_ = ConstJSON(make(chan int))
}

// Every kind has a name for a diagnostic, and an unknown kind says so instead of pretending.
func TestArgKindNames(t *testing.T) {
	for kind, want := range map[argKind]string{
		argKindString:  "string",
		argKindUint:    "integer",
		argKindFloat:   "number",
		argKindBool:    "boolean",
		argKindStrings: "string array",
		argKind(99):    "unknown",
	} {
		if got := kind.String(); got != want {
			t.Errorf("argKind(%d).String() = %q, want %q", kind, got, want)
		}
		if got := kind.decodeHint(); got == "" {
			t.Errorf("argKind(%d).decodeHint() is empty", kind)
		}
	}
}

// The format shorthands are the whole reason the builder is pleasant to read, and each one has
// to project the dialect it names.
func TestFormatShorthandsProjectTheirDialect(t *testing.T) {
	for _, testCase := range []struct {
		arg    *StringArg
		format string
	}{
		{Date("day"), "date"},
		{Time("at"), "time"},
		{DateTime("when"), "date-time"},
		{UUID("id"), "uuid"},
	} {
		name := testCase.arg.argumentName()
		contract := MustArgsContract("t", testCase.arg)
		property := contract.Schema().Properties[name]
		if property.Format != testCase.format || property.Pattern != formatPatterns[testCase.format] {
			t.Errorf("%s = format %q pattern %q", name, property.Format, property.Pattern)
		}
	}
}

var _ = jsontext.Value(nil)
var _ = time.Second

// failingSink fails every event, which is how a downstream that is down behaves.
type failingSink struct {
	err   error
	calls int
}

func (s *failingSink) ItemStarted(context.Context, ItemStartedEvent) error   { s.calls++; return s.err }
func (s *failingSink) ItemDelta(context.Context, ItemDeltaEvent) error       { s.calls++; return s.err }
func (s *failingSink) ItemFinished(context.Context, ItemFinishedEvent) error { s.calls++; return s.err }
func (s *failingSink) LLMCalled(context.Context, LLMCalledEvent) error       { s.calls++; return s.err }

// A sink that is down does not fail the turn by default, but the caller is told about every
// failure, and a caller that wants the turn to fail can say so. Either way the reason is on
// the Turn, because Run reports only what the handler returned.
func TestSinkFailuresAreReportedAndOptionallyFatal(t *testing.T) {
	failure := errors.New("database is down")
	for _, strict := range []bool{false, true} {
		sink := &failingSink{err: failure}
		reported := 0
		turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
			return w.FinalAnswer(ctx, "answer")
		}, RunOptions{
			ConversationID: "sink-failure",
			Sinks:          []Sink{sink},
			StrictSink:     strict,
			OnSinkErr:      func(Sink, error) { reported++ },
		})
		if err != nil {
			t.Fatalf("strict=%t: Run reported the handler's error only: %v", strict, err)
		}
		if reported == 0 || sink.calls == 0 {
			t.Fatalf("strict=%t: %d failures reported out of %d writes", strict, reported, sink.calls)
		}
		if strict {
			if turn.Status != TurnStatusFailed || turn.CloseReason.Code != CloseCodeAgentError {
				t.Fatalf("strict: turn = %+v (%+v)", turn.Status, turn.CloseReason)
			}
			if !errors.Is(turn.CloseReason.Cause, failure) {
				t.Fatalf("strict: cause = %v", turn.CloseReason.Cause)
			}
			continue
		}
		if turn.Status != TurnStatusCompleted {
			t.Fatalf("default: turn = %+v (%+v)", turn.Status, turn.CloseReason)
		}
	}
}

// The final answer can be streamed, and SetFinalText decides the text the turn ends with.
func TestStreamFinalAnswerClosesTheTurn(t *testing.T) {
	var text string
	turn, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		return w.StreamFinalAnswer(ctx, func(stream FinalAnswerStream) error {
			if err := stream.AppendText(ctx, "the answer"); err != nil {
				return err
			}
			stream.SetFinalText("the answer, corrected")
			return nil
		})
	}, RunOptions{ConversationID: "stream-final"})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != TurnStatusCompleted || turn.CloseReason.Code != CloseCodeFinalAnswer {
		t.Fatalf("turn = %+v (%+v)", turn.Status, turn.CloseReason)
	}
	for _, item := range turn.Items {
		if item.Kind == ItemKindFinalAnswer {
			text = item.Text
		}
	}
	if text != "the answer, corrected" {
		t.Fatalf("final answer = %q", text)
	}
}

// CaptureContent is off by default because prompts can carry personal data; when it is on, the
// span gets the prompt and the completion as attributes.
func TestRunCapturesContentWhenAsked(t *testing.T) {
	model := &mockChatModel{chunks: []*Chunk{
		{ContentDelta: "captured"},
		{FinishReason: FinishReasonStop, Usage: &Usage{TotalTokens: 3}},
	}}
	_, err := Run(context.Background(), func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		response, err := StreamLLMToStep(ctx, w, "capture", model, ChatRequest{
			Messages: []Message{{Role: RoleUser, Content: "a question worth capturing"}},
		})
		if err != nil {
			return err
		}
		return w.FinalAnswer(ctx, response.Content)
	}, RunOptions{ConversationID: "capture", CaptureContent: true})
	if err != nil {
		t.Fatal(err)
	}
}

// Subset is how a handler narrows the tools a model may see, and an unknown name is a
// configuration mistake rather than a silently smaller set.
func TestToolRegistrySubset(t *testing.T) {
	first := NewArgsTool(MustArgsContract("first"), "First.", func(context.Context, Args) (string, error) { return "1", nil })
	second := NewArgsTool(MustArgsContract("second"), "Second.", func(context.Context, Args) (string, error) { return "2", nil })
	registry := NewToolRegistry(first, second)

	subset, err := registry.Subset([]string{"second"})
	if err != nil {
		t.Fatal(err)
	}
	if infos, err := subset.InfoList(context.Background()); err != nil || len(infos) != 1 || infos[0].Name != "second" {
		t.Fatalf("subset = %+v (%v)", infos, err)
	}
	// The original is untouched.
	if infos, err := registry.InfoList(context.Background()); err != nil || len(infos) != 2 {
		t.Fatalf("registry = %+v (%v)", infos, err)
	}
	if _, err := registry.Subset([]string{"missing"}); err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("error = %v", err)
	}
}

// A tool that needs the network says so, which is what lets a host run an offline tool set.
func TestToolOptions(t *testing.T) {
	contract := MustArgsContract("offline")
	// A nil option is skipped rather than panicking on the way in.
	tool := NewArgsTool(contract, "Offline.", func(context.Context, Args) (string, error) { return "ok", nil }, nil, WithRequiresNetwork())
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.RequiresNetwork || info.Name != "offline" || info.Parameters == nil {
		t.Fatalf("info = %+v", info)
	}
}

// Providers put a schema into a json_schema parameter as a plain object, and the object form
// round-trips the keywords Loom projects.
func TestStructuredSchemaObject(t *testing.T) {
	if _, err := StructuredSchemaObject(nil); err == nil {
		t.Fatal("a nil schema must be an error")
	}
	object, err := StructuredSchemaObject(&Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"day":  {Type: "string", Format: "date", Pattern: formatPatterns["date"]},
			"note": {Type: "string", AllOf: []*Schema{{Pattern: notBlankPattern}}},
		},
		Required: []string{"day"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if object["type"] != "object" {
		t.Fatalf("object = %#v", object)
	}
	properties, ok := object["properties"].(map[string]any)
	if !ok || properties["day"].(map[string]any)["format"] != "date" {
		t.Fatalf("properties = %#v", object["properties"])
	}
	if required, ok := object["required"].([]any); !ok || len(required) != 1 {
		t.Fatalf("required = %#v", object["required"])
	}
}

// A structured-output failure names how many attempts were made and stays unwrappable, because
// a caller may want to test for the error underneath.
func TestStructuredOutputError(t *testing.T) {
	cause := errors.New("not json")
	failure := &StructuredOutputError{Attempt: 2, Content: "not json", Err: cause}
	if !strings.Contains(failure.Error(), "attempt 2") {
		t.Fatalf("message = %q", failure.Error())
	}
	if !errors.Is(failure, cause) {
		t.Fatal("the cause is not reachable")
	}
}

// Every builder method, declared once, because each one is public API a caller writes: the
// numeric and boolean handles take the same constraints as the string ones.
func TestBuilderSurface(t *testing.T) {
	count := Uint("count").Required().Min(1).Max(10).ExclusiveMax(11).Example(3).Desc("Count.")
	ratio := Float("ratio").Required().Min(0).Max(1).ExclusiveMin(-0.5).Example(0.5).Desc("Ratio.")
	enabled := Bool("enabled").Required().Example(true).Desc("Enabled.")
	names := Strings("names").Required().MinItems(1).MaxItems(3).Unique().Example("a").Desc("Names.")
	// A field validator sees the decoded value, which is what makes it typed.
	count.Validate(func(_ context.Context, value uint64) error {
		if value == 0 {
			return InvalidOn(count, "count must be positive")
		}
		return nil
	})

	contract := MustArgsContract("surface", count, ratio, enabled, names)
	schema := contract.Schema()
	if schema.Properties["count"].Minimum == nil || schema.Properties["ratio"].ExclusiveMinimum == nil {
		t.Fatalf("numeric constraints = %+v", schema.Properties)
	}
	if schema.Properties["names"].UniqueItems != true {
		t.Fatalf("array constraints = %+v", schema.Properties["names"])
	}
	for name, wantType := range map[string]string{"count": "integer", "ratio": "number", "enabled": "boolean", "names": "array"} {
		if got := schema.Properties[name].Type; got != wantType {
			t.Errorf("%s = %q, want %q", name, got, wantType)
		}
	}
	if len(schema.Required) != 4 {
		t.Fatalf("required = %v", schema.Required)
	}
	// The declared examples are a complete call, so they validate.
	if _, err := contract.Decode(`{"count":3,"ratio":0.5,"enabled":true,"names":["a"]}`); err != nil {
		t.Fatalf("a valid call failed: %v", err)
	}
	// The field validator reports a model-facing problem with the handle, so the diagnostic
	// names the field.
	_, err := contract.Decode(`{"count":0,"ratio":0.5,"enabled":true,"names":["a"]}`)
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) || argumentError.Issues[0].Field != "count" {
		t.Fatalf("error = %v", err)
	}
}
