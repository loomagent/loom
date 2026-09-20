package loom

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type webSearchArgs struct {
	contract *ArgsContract
	query    *StringArg
	typ      *StringArg
	dateFrom *StringArg
	dateTo   *StringArg
}

func newWebSearchArgs() webSearchArgs {
	query := String("query").Required().MinLen(1).MaxLen(40).Desc("Search query.")
	typ := Enum("type", "search", "news").Desc("Result type.")
	dateFrom := Date("date_from").Desc("Optional lower bound.")
	dateTo := Date("date_to").Desc("Optional upper bound.")
	return webSearchArgs{
		contract: MustArgsContract("web_search",
			query, typ, dateFrom, dateTo,
			Cross(dateFrom, dateTo).Using(validateDateRange),
		),
		query:    query,
		typ:      typ,
		dateFrom: dateFrom,
		dateTo:   dateTo,
	}
}

// validateDateRange is a named cross-field rule. The contract names its
// dependencies through the handles, so the rule can be unit tested directly.
func validateDateRange(_ context.Context, from, to string) error {
	if from != "" && to != "" && from > to {
		return InvalidAt("date_to", "date_to (%s) must not precede date_from (%s)", to, from)
	}
	return nil
}

func TestArgsContractSchema(t *testing.T) {
	schema := newWebSearchArgs().contract.Schema()

	if schema.Type != "object" {
		t.Fatalf("schema type = %q, want object", schema.Type)
	}
	if got, want := schema.Required, []string{"query"}; !slices.Equal(got, want) {
		t.Fatalf("required = %v, want %v", got, want)
	}
	if schema.AdditionalProperties == nil || schema.AdditionalProperties.Not == nil {
		t.Fatal("additionalProperties must reject unknown arguments")
	}
	if got, want := schema.Properties["type"].Enum, []any{"search", "news"}; !slices.Equal(got, want) {
		t.Fatalf("type enum = %v, want %v", got, want)
	}
	// A declared format must also project the shape pattern, so providers that
	// ignore format still constrain the value.
	date := schema.Properties["date_from"]
	if date.Format != "date" {
		t.Fatalf("date_from format = %q, want date", date.Format)
	}
	if date.Pattern != formatPatterns["date"] {
		t.Fatalf("date_from pattern = %q, want %q", date.Pattern, formatPatterns["date"])
	}
	if got := schema.Properties["query"].MinLength; got == nil || *got != 1 {
		t.Fatalf("query minLength = %v, want 1", got)
	}
}

func TestArgsContractDecodeValid(t *testing.T) {
	ws := newWebSearchArgs()
	args, err := ws.contract.Decode(`{"query":"go generics","type":"news","date_from":"2026-08-01","date_to":"2026-08-20"}`)
	if err != nil {
		t.Fatalf("Decode() = %v, want nil", err)
	}
	if got := ws.query.Get(args); got != "go generics" {
		t.Fatalf("query = %q", got)
	}
	if !ws.typ.Present(args) || ws.typ.Get(args) != "news" {
		t.Fatalf("type = %q, present=%v", ws.typ.Get(args), ws.typ.Present(args))
	}
}

func TestArgsContractDecodeOptionalOmitted(t *testing.T) {
	ws := newWebSearchArgs()
	args, err := ws.contract.Decode(`{"query":"go"}`)
	if err != nil {
		t.Fatalf("Decode() = %v, want nil", err)
	}
	if ws.typ.Present(args) {
		t.Fatal("type should be absent")
	}
	if got := ws.typ.Get(args); got != "" {
		t.Fatalf("omitted type = %q, want empty", got)
	}
}

func TestArgsContractDecodeSchemaViolations(t *testing.T) {
	ws := newWebSearchArgs()
	for _, raw := range []string{
		`{}`,                              // missing required
		`{"query":"go","type":"scholar"}`, // enum
		`{"query":"go","extra":1}`,        // additional property
		`{"query":"go","date_from":"nope"}`,
		`{"query":123}`, // type
	} {
		_, err := ws.contract.Decode(raw)
		if err == nil {
			t.Fatalf("Decode(%s) = nil, want error", raw)
		}
		var argumentError *ToolArgumentError
		if !errors.As(err, &argumentError) {
			t.Fatalf("Decode(%s) error type = %T, want *ToolArgumentError", raw, err)
		}
		if argumentError.Kind == ToolArgumentErrorCustom {
			t.Fatalf("Decode(%s) kind = custom, want schema stage", raw)
		}
		if len(argumentError.Issues) == 0 {
			t.Fatalf("Decode(%s) produced no issues", raw)
		}
	}
}

func TestArgsContractFieldValidator(t *testing.T) {
	srcID := String("src_id").Required().Pattern(`^SRC-\d+$`).Validate(validateSrcID)
	contract := MustArgsContract("read_reference", srcID)
	if _, err := contract.Decode(`{"src_id":"SRC-1"}`); err != nil {
		t.Fatalf("valid id rejected: %v", err)
	}
	_, err := contract.Decode(`{"src_id":"SRC-404"}`)
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatalf("error type = %T, want *ToolArgumentError", err)
	}
	if argumentError.Kind != ToolArgumentErrorCustom {
		t.Fatalf("kind = %q, want custom", argumentError.Kind)
	}
	if got, want := argumentError.Issues[0].Field, "src_id"; got != want {
		t.Fatalf("issue field = %q, want %q", got, want)
	}
	if argumentError.Issues[0].Rule != "custom" {
		t.Fatalf("issue rule = %q, want custom", argumentError.Issues[0].Rule)
	}
}

func validateSrcID(_ context.Context, value string) error {
	if value == "SRC-404" {
		return Invalid("unknown source id %q; use an id from the source index", value)
	}
	return nil
}

func TestArgsContractValidatorsCollectEveryProblem(t *testing.T) {
	from := String("from").Validate(func(_ context.Context, value string) error {
		if value == "bad" {
			return Invalid("from is bad")
		}
		return nil
	})
	to := String("to").Validate(func(_ context.Context, value string) error {
		if value == "bad" {
			return errors.Join(Invalid("to is bad"), Invalid("to is also empty"))
		}
		return nil
	})
	contract := MustArgsContract("search", from, to)
	_, err := contract.Decode(`{"from":"bad","to":"bad"}`)
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatalf("error type = %T, want *ToolArgumentError", err)
	}
	fields := map[string]int{}
	for _, issue := range argumentError.Issues {
		fields[issue.Field]++
	}
	if fields["from"] != 1 || fields["to"] != 2 {
		t.Fatalf("issues = %+v, want one for from and two for to", argumentError.Issues)
	}
}

func TestArgsContractSchemaGateStopsValidators(t *testing.T) {
	ran := false
	q := String("q").Required().Validate(func(_ context.Context, _ string) error {
		ran = true
		return nil
	})
	contract := MustArgsContract("gate", q)
	if _, err := contract.Decode(`{}`); err == nil {
		t.Fatal("missing required argument accepted")
	}
	if ran {
		t.Fatal("field validator ran despite a schema violation")
	}
}

func TestArgsContractInternalValidatorError(t *testing.T) {
	internal := errors.New("database unavailable")
	q := String("q").Validate(func(_ context.Context, _ string) error { return internal })
	contract := MustArgsContract("internal", q)
	_, err := contract.Decode(`{"q":"x"}`)
	if err == nil {
		t.Fatal("internal failure treated as success")
	}
	var argumentError *ToolArgumentError
	if errors.As(err, &argumentError) {
		t.Fatalf("internal failure surfaced as a model-facing error: %v", err)
	}
	if !errors.Is(err, internal) {
		t.Fatalf("error %v does not wrap the internal failure", err)
	}
}

func TestArgsContractWholeRuleTargetsField(t *testing.T) {
	ws := newWebSearchArgs()
	_, err := ws.contract.Decode(`{"query":"go","date_from":"2026-08-20","date_to":"2026-08-01"}`)
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatalf("error type = %T, want *ToolArgumentError", err)
	}
	if argumentError.Kind != ToolArgumentErrorCustom {
		t.Fatalf("kind = %q, want custom", argumentError.Kind)
	}
	if got, want := argumentError.Issues[0].Field, "date_to"; got != want {
		t.Fatalf("issue field = %q, want %q", got, want)
	}
}

func TestArgsContractWholeRuleSkippedWhenFieldsAbsent(t *testing.T) {
	ran := 0
	a := String("a").Desc("Optional a.")
	b := String("b").Desc("Optional b.")
	contract := MustArgsContract("optional_pair", a, b,
		Cross(a, b).Using(func(_ context.Context, _, _ string) error {
			ran++
			return nil
		}),
	)
	if _, err := contract.Decode(`{}`); err != nil {
		t.Fatalf("Decode({}) = %v", err)
	}
	if ran != 0 {
		t.Fatalf("whole-call rule ran %d times with no dependency present", ran)
	}
	if _, err := contract.Decode(`{"a":"x"}`); err != nil {
		t.Fatalf("Decode({a}) = %v", err)
	}
	if ran != 1 {
		t.Fatalf("whole-call rule ran %d times, want 1", ran)
	}
}

func TestArgsContractWholeRuleRejectsUndeclaredHandle(t *testing.T) {
	declared := String("a").Desc("A.")
	undeclared := String("b").Desc("B.")
	if _, err := NewArgsContract("bad", declared,
		Cross(declared, undeclared).Using(func(context.Context, string, string) error { return nil }),
	); err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("undeclared dependency error = %v", err)
	}
}

func TestUintArgumentRange(t *testing.T) {
	n := Uint("n")
	contract := MustArgsContract("count", n)
	schema := contract.Schema()
	minimum := schema.Properties["n"].Minimum
	if minimum == nil || *minimum != 0 {
		t.Fatalf("unsigned argument minimum = %v, want 0", minimum)
	}

	bounded := Uint("n").Min(2).Max(9)
	contract = MustArgsContract("bounded", bounded)
	schema = contract.Schema()
	if got := schema.Properties["n"].Minimum; got == nil || *got != 2 {
		t.Fatalf("minimum = %v, want 2", got)
	}
	if got := schema.Properties["n"].Maximum; got == nil || *got != 9 {
		t.Fatalf("maximum = %v, want 9", got)
	}
	for _, raw := range []string{`{"n":-1}`, `{"n":1.5}`} {
		if _, err := contract.Decode(raw); err == nil {
			t.Fatalf("Decode(%s) accepted a value outside the declared range", raw)
		}
	}
}

func TestUintArgumentRejectsValuesThatDoNotFit(t *testing.T) {
	n := Uint("n")
	contract := MustArgsContract("count", n)
	// These are integers to JSON Schema but exceed uint64 or are not written as
	// integers, so they must be reported as type problems rather than reaching
	// the handle.
	for _, raw := range []string{`{"n":1e3}`, `{"n":18446744073709551616}`} {
		_, err := contract.Decode(raw)
		var argumentError *ToolArgumentError
		if !errors.As(err, &argumentError) {
			t.Fatalf("Decode(%s) error type = %T, want *ToolArgumentError", raw, err)
		}
		if !strings.Contains(err.Error(), "n") {
			t.Fatalf("Decode(%s) error does not name the field: %v", raw, err)
		}
	}
}

func TestUintHandlePreservesFullRange(t *testing.T) {
	n := Uint("n")
	contract := MustArgsContract("count", n)
	args, err := contract.Decode(`{"n":18446744073709551615}`)
	if err != nil {
		t.Fatalf("Decode() = %v", err)
	}
	if got := n.Get(args); got != 18446744073709551615 {
		t.Fatalf("n = %d, want 18446744073709551615", got)
	}
}

func TestArgsHasPanicsOnUndeclared(t *testing.T) {
	contract := MustArgsContract("t", String("q").Desc("Q."))
	args, err := contract.Decode(`{"q":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !args.Has("q") {
		t.Fatal("q should be present")
	}
	assertPanics(t, func() { args.Has("missing") })
}

func TestNewArgsToolEndToEnd(t *testing.T) {
	ws := newWebSearchArgs()
	tool := NewArgsTool(ws.contract, "Search the web.", func(_ context.Context, args Args) (string, error) {
		return `"` + ws.query.Get(args) + `"`, nil
	})

	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "web_search" || info.Parameters == nil {
		t.Fatalf("Info() = %+v", info)
	}
	result, err := tool.Invoke(context.Background(), `{"query":"go"}`)
	if err != nil {
		t.Fatalf("Invoke() = %v", err)
	}
	if result != `"go"` {
		t.Fatalf("result = %s", result)
	}
	if _, err := tool.Invoke(context.Background(), `{}`); err == nil {
		t.Fatal("Invoke() accepted arguments that violate the contract")
	}
}

// A rule that closes over its handles can point an error at a field without a
// string: InvalidOn takes the handle's name by construction.
func TestArgsContractWholeRuleTargetsFieldViaHandle(t *testing.T) {
	dateFrom := Date("date_from").Desc("From.")
	dateTo := Date("date_to").Desc("To.")
	rule := func(_ context.Context, from, to string) error {
		if from != "" && to != "" && from > to {
			return InvalidOn(dateTo, "date_to (%s) must not precede date_from (%s)", to, from)
		}
		return nil
	}
	contract := MustArgsContract("range", dateFrom, dateTo, Cross(dateFrom, dateTo).Using(rule))
	_, err := contract.Decode(`{"date_from":"2026-08-20","date_to":"2026-08-01"}`)
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatalf("error type = %T, want *ToolArgumentError", err)
	}
	if got, want := argumentError.Issues[0].Field, "date_to"; got != want {
		t.Fatalf("issue field = %q, want %q", got, want)
	}
}

// A rule that points an error at an argument the contract does not declare is a
// programming mistake, not a model mistake, so it must not reach the model as a
// correction request with a bogus field.
func TestArgsContractMisdirectedFieldIsInternal(t *testing.T) {
	a := String("a").Desc("A.")
	contract := MustArgsContract("misdirect", a,
		Cross(a, a).Using(func(context.Context, string, string) error {
			return InvalidAt("missing", "oops")
		}),
	)
	_, err := contract.Decode(`{"a":"x"}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	var argumentError *ToolArgumentError
	if errors.As(err, &argumentError) {
		t.Fatalf("a misdirected field surfaced as a model-facing error: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown argument") {
		t.Fatalf("error = %v, want it to name the unknown argument", err)
	}
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}
