package loom

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

type integrityLog struct {
	mu     sync.Mutex
	events []TextIntegrityObservation
}

func (l *integrityLog) observe(_ context.Context, event TextIntegrityObservation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}
func (l *integrityLog) snapshot() []TextIntegrityObservation {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]TextIntegrityObservation(nil), l.events...)
}

type integrityFixtureStream struct {
	chunks           []*Chunk
	index            int
	endErr, closeErr error
	closes           int
}

func (s *integrityFixtureStream) Recv() (*Chunk, error) {
	if s.index < len(s.chunks) {
		v := s.chunks[s.index]
		s.index++
		return v, nil
	}
	if s.endErr != nil {
		return nil, s.endErr
	}
	return nil, io.EOF
}
func (s *integrityFixtureStream) Close() error { s.closes++; return s.closeErr }

func TestTextIntegrityAllModelFieldsAreReadOnly(t *testing.T) {
	t.Parallel()
	log := &integrityLog{}
	ctx := WithTextIntegrityObserver(t.Context(), log.observe)
	response := &ChatResponse{Content: `{"answer":"\ufffd"}`, ReasoningContent: "思考出现�", ReasoningDetails: []byte(`{"text":"损坏�"}`), ToolCalls: []ToolCall{{ID: "call�", Name: "lookup�", Arguments: `{"city":"\ufffd"}`}}, Model: "reported-model", Usage: Usage{TotalTokens: 9}}
	before, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	info := ModelInfo{Provider: "test", RequestModel: "requested-model", Ref: ModelRef{InstanceID: "db-model"}}
	got, err := ObserveChat(ctx, info, func(context.Context) (*ChatResponse, error) { return response, nil })
	after, marshalErr := json.Marshal(response)
	if err != nil || marshalErr != nil || got != response || string(before) != string(after) {
		t.Fatalf("observer changed response: %v %v", err, marshalErr)
	}
	events := log.snapshot()
	if len(events) != 6 {
		t.Fatalf("want all six fields, got %+v", events)
	}
	fields := map[string]bool{}
	for _, event := range events {
		fields[event.Field] = true
		if event.Invocation.ModelInfo != info || event.Invocation.CallID == "" || event.ReportedModel != "reported-model" || event.Partial || event.Source != "model" {
			t.Fatalf("lost identity: %+v", event)
		}
	}
	for _, field := range []string{"content.answer", "reasoning_content", "reasoning_details.text", "tool_calls[0].id", "tool_calls[0].name", "tool_calls[0].arguments.city"} {
		if !fields[field] {
			t.Errorf("missing %s", field)
		}
	}
}

func TestTextIntegrityDetectsSupportedEncodingProblems(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text, kind string
		partial          bool
	}{
		{"replacement", "正常中文�", "replacement_character", false},
		{"invalid byte", "a\xffb", "invalid_utf8", false},
		{"truncated EOF", "a\xe4", "invalid_utf8", false},
		{"partial tail", "a\xe4", "", true},
		{"invalid interior partial", "a\xffb\xe4", "invalid_utf8", true},
		{"CP1252", "æ�Žè€�å¸ˆä¸�æ˜¯ä½ è€�å¸ˆ", "cp1252_mojibake", false},
		{"Latin1 controls", "æ\u009d\u008eè\u0080\u0081", "cp1252_mojibake", false},
		{"normal unicode", "«déjà» vu café 🇺🇸 李老师不是你老师", "", false},
		{"one CJK character", "äºº", "cp1252_mojibake", false},
		{"lossy CJK character", "ä¸\ufffd", "cp1252_mojibake", false}, // 不 = E4 B8 8D; 0x8D became U+FFFD
		{"one emoji", "ðŸ‘€", "cp1252_mojibake", false},
		{"one apostrophe in English", "Itâ€™s fine and weâ€™re here", "cp1252_mojibake", false},
		{"Latin-1 letter", "JosÃ©", "cp1252_mojibake", false},
		// Accented names followed by CP1252 punctuation in normal output (dev agent corpus).
		{"name in Chinese quotes", "打击“FETÖ”行动，另有法院以“FETÖ”成员罪判刑", "", false},
		{"name and en dash", "IED on Sévaré–Konna road; Sévaré–Konna axis", "", false},
		{"names in quotes", "Santos（绰号“Pará”）与“José”同日遇袭", "", false},
		{"accent before quote and ellipsis", "perché”… 意大利语引用", "", false},
		{"Nordic and German", "27 år gammel. Använd inte. Grüße aus Köln, daß", "", false},
		{"symbols", "¿Qué pasó? ¡Olé! °C ±5 ×10³ “quoted” — dash… •", "", false},
		{"one lone pair", "æœ solo", "", false},
		{"surrogate is not evidence", "Así\u00a0€5", "", false}, // í NBSP € = ED A0 80
		{"empty", "", "", false},
		{"JSON escaped", `{"value":"\ufffd"}`, "replacement_character", false},
		{"JSON key", `{"\ufffd":"ok","items":["ok",null,1]}`, "replacement_character", false},
		{"JSON scalar", "123", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log := &integrityLog{}
			inspectIntegrityValue(tc.text, "test", tc.partial, integrityReporter(t.Context(), log.observe, TextIntegrityObservation{}))
			events := log.snapshot()
			found := false
			for _, e := range events {
				if e.Kind == tc.kind {
					found = true
				}
				if utf8.RuneCountInString(e.Sample) > 64 {
					t.Fatalf("unbounded sample: %+v", e)
				}
			}
			if tc.kind == "" && len(events) != 0 || tc.kind != "" && !found {
				t.Fatalf("unexpected findings: %+v", events)
			}
		})
	}
}

func TestTextIntegritySamplesAreBoundedAndIncludeFailure(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("前", 100) + "�" + strings.Repeat("后", 10000)
	got := integritySample(text, len(strings.Repeat("前", 100)))
	if !strings.Contains(got, "�") || utf8.RuneCountInString(got) != 64 {
		t.Fatalf("sample=%q", got)
	}
}

func TestTextIntegrityStreamJoinsEachChannelBeforeInspection(t *testing.T) {
	t.Parallel()
	log := &integrityLog{}
	ctx := WithTextIntegrityObserver(t.Context(), log.observe)
	chunks := []*Chunk{
		{ContentDelta: "\xe4", ReasoningContentDelta: "正常", ToolCallDeltas: []ToolCallDelta{{Index: 2, ID: "call", Name: "lookup", Arguments: `{"value":"\u`}}},
		nil,
		{ContentDelta: "\xbd\xa0", ReasoningDetails: []byte(`{"text":"thinking�"}`), ToolCallDeltas: []ToolCallDelta{{Index: 2, Arguments: `fffd"}`}}},
		{ContentDelta: "好", ReasoningContentDelta: "思考", ReasoningDetails: []byte(`{"text":"thinking�"}`), Model: "actual"},
	}
	raw := &integrityFixtureStream{chunks: chunks}
	stream, err := ObserveStream(ctx, ModelInfo{Provider: "fixture"}, func(context.Context) (Stream, error) { return raw, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range chunks {
		got, err := stream.Recv()
		if err != nil || got != want {
			t.Fatal("chunk identity changed")
		}
		if len(log.snapshot()) != 0 {
			t.Fatal("diagnostics ran before assembly")
		}
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("EOF changed: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	events := log.snapshot()
	if len(events) != 2 {
		t.Fatalf("want tool argument + reasoning detail, got %+v", events)
	}
	for _, e := range events {
		if e.Kind != "replacement_character" || e.Partial || e.ReportedModel != "actual" {
			t.Fatalf("spurious UTF8 finding: %+v", e)
		}
	}
}

func TestTextIntegrityStreamErrorsAndCloseKeepTheirIdentity(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"close", "recv error", "wrapped EOF"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			log := &integrityLog{}
			ctx := WithTextIntegrityObserver(t.Context(), log.observe)
			sentinel := errors.New("original error")
			raw := &integrityFixtureStream{chunks: []*Chunk{{ContentDelta: "损坏�\xe4"}}, closeErr: sentinel}
			if mode == "recv error" {
				raw.endErr = sentinel
			}
			if mode == "wrapped EOF" {
				raw.endErr = errors.Join(io.EOF)
			}
			stream, err := ObserveStream(ctx, ModelInfo{}, func(context.Context) (Stream, error) { return raw, nil })
			if err != nil {
				t.Fatal(err)
			}
			_, _ = stream.Recv()
			if mode != "close" {
				_, err = stream.Recv()
				if err != raw.endErr {
					t.Fatalf("receive error changed: %v", err)
				}
			}
			if err = stream.Close(); err != sentinel {
				t.Fatalf("close error changed: %v", err)
			}
			_ = stream.Close()
			want := 1
			if mode == "wrapped EOF" {
				want = 2
			}
			if events := log.snapshot(); len(events) != want {
				t.Fatalf("wrong partial handling or duplicate close findings: %+v", events)
			}
		})
	}
}

func TestTextIntegrityNestedAndConcurrentModelCalls(t *testing.T) {
	t.Parallel()
	log := &integrityLog{}
	ctx := WithTextIntegrityObserver(t.Context(), log.observe)
	info := ModelInfo{Provider: "test"}
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() {
			response, err := ObserveChat(ctx, info, func(inner context.Context) (*ChatResponse, error) {
				return ObserveChat(inner, info, func(context.Context) (*ChatResponse, error) { return &ChatResponse{Content: "坏�"}, nil })
			})
			if err != nil || response.Content != "坏�" {
				t.Errorf("changed call result: %v", err)
			}
		})
	}
	workers.Wait()
	if n := len(log.snapshot()); n != 20 {
		t.Fatalf("nested observers duplicated records: %d", n)
	}
	log = &integrityLog{}
	ctx = WithTextIntegrityObserver(t.Context(), log.observe)
	raw := &integrityFixtureStream{chunks: []*Chunk{{ContentDelta: "�"}}}
	stream, err := ObserveStream(ctx, info, func(inner context.Context) (Stream, error) {
		return ObserveStream(inner, info, func(context.Context) (Stream, error) { return raw, nil })
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = stream.Recv()
	_, _ = stream.Recv()
	_ = stream.Close()
	if len(log.snapshot()) != 1 {
		t.Fatal("nested streams reported twice")
	}
}

func TestTextIntegrityObserverPanicDoesNotChangeBusinessErrors(t *testing.T) {
	t.Parallel()
	ctx := WithTextIntegrityObserver(t.Context(), func(context.Context, TextIntegrityObservation) { panic("diagnostic failure") })
	sentinel := errors.New("original provider error")
	response := &ChatResponse{Content: "坏�"}
	got, err := ObserveChat(ctx, ModelInfo{}, func(context.Context) (*ChatResponse, error) { return response, sentinel })
	if got != response || err != sentinel {
		t.Fatalf("observer changed result: %v", err)
	}
}

func TestTextIntegrityDefaultObserverAndExplicitDisable(t *testing.T) {
	log := &integrityLog{}
	SetTextIntegrityObserver(log.observe)
	t.Cleanup(func() { SetTextIntegrityObserver(nil) })
	call := func(context.Context) (*ChatResponse, error) { return &ChatResponse{Content: "default�"}, nil }
	_, _ = ObserveChat(t.Context(), ModelInfo{}, call)
	_, _ = ObserveChat(WithTextIntegrityObserver(t.Context(), nil), ModelInfo{}, call)
	if len(log.snapshot()) != 1 {
		t.Fatal("default/override contract broken")
	}
	SetTextIntegrityObserver(nil)
	raw := &integrityFixtureStream{}
	stream, err := ObserveStream(t.Context(), ModelInfo{}, func(context.Context) (Stream, error) { return raw, nil })
	if err != nil || stream != raw {
		t.Fatal("disabled observer wrapped stream")
	}
}

func TestTextIntegrityRuntimeItemsAndToolResultsAreUnchanged(t *testing.T) {
	t.Parallel()
	log := &integrityLog{}
	ctx := WithTextIntegrityObserver(t.Context(), log.observe)
	calls := []ToolCall{{ID: "call", Name: "lookup", Arguments: `{"query":"\ufffd"}`}}
	turn, err := Run(ctx, func(ctx context.Context, w TurnWriter, _ []Turn, _ UserMessage) error {
		if err := w.WriteReasoning(ctx, "思考", "reasoning�"); err != nil {
			return err
		}
		if err := w.WriteToolCall(ctx, "lookup", calls[0]); err != nil {
			return err
		}
		if err := w.WriteToolResult(ctx, "lookup", ToolResult{CallID: "call", ToolName: "lookup", Output: `{"result":"\ufffd"}`, Err: &ItemError{Code: "error�", Message: "failure�"}}); err != nil {
			return err
		}
		return w.FinalAnswer(ctx, "answer�")
	}, RunOptions{ConversationID: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Items[0].Text != "reasoning�" || turn.Items[1].Arguments != calls[0].Arguments || turn.Items[2].Output != `{"result":"\ufffd"}` || turn.Items[3].Text != "answer�" {
		t.Fatal("observer modified items")
	}
	events := log.snapshot()
	sources := map[string]bool{}
	for _, e := range events {
		sources[e.Source] = true
		if e.ConversationID != "conversation" || e.ItemPath == "" {
			t.Fatalf("lost runtime origin: %+v", e)
		}
	}
	if len(events) != 6 || !reflect.DeepEqual(sources, map[string]bool{"runtime_item": true, "tool_result": true}) {
		t.Fatalf("missing item text: %+v", events)
	}
}

type integrityBlockingStream struct {
	calls   atomic.Int32
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (s *integrityBlockingStream) Recv() (*Chunk, error) {
	if s.calls.Add(1) == 1 {
		return &Chunk{ContentDelta: "before close�"}, nil
	}
	close(s.started)
	<-s.closed
	return &Chunk{ContentDelta: "late data�"}, io.ErrClosedPipe
}

func (s *integrityBlockingStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func TestTextIntegrityCloseWhileReceivingReportsOnce(t *testing.T) {
	t.Parallel()
	log := &integrityLog{}
	ctx := WithTextIntegrityObserver(t.Context(), log.observe)
	raw := &integrityBlockingStream{started: make(chan struct{}), closed: make(chan struct{})}
	stream, err := ObserveStream(ctx, ModelInfo{}, func(context.Context) (Stream, error) { return raw, nil })
	if err != nil {
		t.Fatal(err)
	}
	_, _ = stream.Recv()
	received := make(chan error, 1)
	go func() { _, err := stream.Recv(); received <- err }()
	<-raw.started
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-received; err != io.ErrClosedPipe {
		t.Fatalf("receive error changed: %v", err)
	}
	if events := log.snapshot(); len(events) != 1 || !events[0].Partial {
		t.Fatalf("concurrent close duplicated findings: %+v", events)
	}
}

func TestTextIntegrityCloseAfterFinishReasonIsComplete(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		chunks  []*Chunk
		partial bool
	}{
		{"finish reason seen", []*Chunk{{ContentDelta: "坏�"}, {FinishReason: FinishReasonStop}}, false},
		{"no finish reason", []*Chunk{{ContentDelta: "坏�"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log := &integrityLog{}
			ctx := WithTextIntegrityObserver(t.Context(), log.observe)
			stream, err := ObserveStream(ctx, ModelInfo{}, func(context.Context) (Stream, error) {
				return &integrityFixtureStream{chunks: tc.chunks}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for range tc.chunks {
				if _, err := stream.Recv(); err != nil {
					t.Fatal(err)
				}
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			events := log.snapshot()
			if len(events) != 1 || events[0].Partial != tc.partial {
				t.Fatalf("want one finding with partial=%v, got %+v", tc.partial, events)
			}
		})
	}
}

func TestTextIntegrityNilAndFailedModelResponses(t *testing.T) {
	t.Parallel()
	log := &integrityLog{}
	ctx := WithTextIntegrityObserver(t.Context(), log.observe)
	sentinel := errors.New("provider failure")
	response, err := ObserveChat(ctx, ModelInfo{}, func(context.Context) (*ChatResponse, error) { return nil, sentinel })
	if response != nil || err != sentinel {
		t.Fatal("nil response/error changed")
	}
	stream, err := ObserveStream(ctx, ModelInfo{}, func(context.Context) (Stream, error) { return nil, sentinel })
	if stream != nil || err != sentinel {
		t.Fatal("stream opening failure changed")
	}
	if len(log.snapshot()) != 0 {
		t.Fatal("empty failed response reported a finding")
	}
}
