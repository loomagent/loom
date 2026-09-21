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
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
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
	if _, ok := errors.AsType[*ToolArgumentError](err); ok {
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
		if _, ok := errors.AsType[*ToolArgumentError](err); !ok {
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

// A strict bound must reach both the model and the local check: it is projected
// into the schema and enforced on whatever the model sends.
func TestNumericExclusiveBounds(t *testing.T) {
	count := Uint("n").ExclusiveMin(0).ExclusiveMax(10)
	contract := MustArgsContract("count", count)
	schema := contract.Schema()
	if got := schema.Properties["n"].ExclusiveMinimum; got == nil || *got != 0 {
		t.Fatalf("exclusiveMinimum = %v, want 0", got)
	}
	if got := schema.Properties["n"].ExclusiveMaximum; got == nil || *got != 10 {
		t.Fatalf("exclusiveMaximum = %v, want 10", got)
	}
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"n":0}`, false},  // equal to the strict lower bound
		{`{"n":10}`, false}, // equal to the strict upper bound
		{`{"n":1}`, true},
		{`{"n":9}`, true},
	} {
		_, err := contract.Decode(tc.raw)
		if tc.valid && err != nil {
			t.Fatalf("Decode(%s) = %v, want accepted", tc.raw, err)
		}
		if !tc.valid && err == nil {
			t.Fatalf("Decode(%s) accepted a value on a strict bound", tc.raw)
		}
	}

	ratio := Float("f").ExclusiveMin(0.5)
	ratioContract := MustArgsContract("ratio", ratio)
	if _, err := ratioContract.Decode(`{"f":0.5}`); err == nil {
		t.Fatal("value equal to the strict lower bound accepted")
	}
	if _, err := ratioContract.Decode(`{"f":0.6}`); err != nil {
		t.Fatalf("value above the strict lower bound rejected: %v", err)
	}
}

// A format the model is told about must also be enforced: either Loom patterns
// it, or the author supplies a pattern. Otherwise the contract would advertise a
// constraint it never checks.
func TestFormatMustBeEnforced(t *testing.T) {
	if _, err := NewArgsContract("unpatched", String("x").Format("email")); err == nil || !strings.Contains(err.Error(), "email") {
		t.Fatalf("format without a pattern = %v, want an error naming the format", err)
	}
	if _, err := NewArgsContract("patched", String("x").Format("email").Pattern(`^[^@]+@[^@]+$`)); err != nil {
		t.Fatalf("format with an explicit pattern rejected: %v", err)
	}
	if _, err := NewArgsContract("known", Date("d")); err != nil {
		t.Fatalf("a format Loom patterns rejected: %v", err)
	}
}

// Reading through a handle the contract never declared is a programming error. A zero
// value would be indistinguishable from an optional argument the model omitted, so every
// read path panics instead of inventing one.
func TestArgsReadPanicsOnUndeclared(t *testing.T) {
	q := String("q").Desc("Q.")
	contract := MustArgsContract("t", q)
	args, err := contract.Decode(`{"q":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get(args) != "x" || !q.Present(args) || string(args.RawJSON("q")) != `"x"` {
		t.Fatal("reading a declared argument failed")
	}

	other := String("other").Desc("Other.")
	assertPanics(t, func() { other.Get(args) })
	assertPanics(t, func() { other.Present(args) })
	assertPanics(t, func() { args.RawJSON("other") })
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
	if _, ok := errors.AsType[*ToolArgumentError](err); ok {
		t.Fatalf("a misdirected field surfaced as a model-facing error: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown argument") {
		t.Fatalf("error = %v, want it to name the unknown argument", err)
	}
}

func TestArgsKeepsOriginalJSON(t *testing.T) {
	n := Uint("n")
	other := String("other").Desc("Optional.")
	contract := MustArgsContract("raw", n, other)
	// A value beyond float64 precision, so a round-trip through a generic Go
	// value would not survive.
	const payload = `{"n":9007199254740993}`
	args, err := contract.Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(args.JSON()); got != payload {
		t.Fatalf("JSON() = %q, want %q", got, payload)
	}
	if got := string(args.RawJSON("n")); got != "9007199254740993" {
		t.Fatalf("RawJSON(n) = %q", got)
	}
	if got := args.RawJSON("other"); got != nil {
		t.Fatalf("RawJSON of an omitted argument = %q, want nil", got)
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

// NotBlank is the standard schema idiom rather than a private rule, so the model sees it
// in the schema it receives, before it calls anything.
func TestNotBlankIsProjectedIntoTheSchema(t *testing.T) {
	note := String("note").NotBlank().Desc("Note.")
	contract := MustArgsContract("t", note)
	if got := contract.Schema().Properties["note"].Pattern; got != notBlankPattern {
		t.Fatalf("schema pattern = %q", got)
	}
	// A pattern says nothing about presence, so an omitted optional argument still passes.
	if _, err := contract.Decode(`{}`); err != nil {
		t.Fatalf("an omitted optional argument must pass: %v", err)
	}
	for _, blank := range []string{`{"note":""}`, `{"note":"   "}`, `{"note":"\t\n"}`} {
		if _, err := contract.Decode(blank); err == nil {
			t.Fatalf("%s must be rejected", blank)
		}
	}
	if _, err := contract.Decode(`{"note":"x"}`); err != nil {
		t.Fatalf("a non-blank value must pass: %v", err)
	}
}

// Required is presence and NotBlank is content. Only together do they say "the model must
// send something meaningful", so each one alone is still meaningful.
func TestRequiredAndNotBlankCoverDifferentThings(t *testing.T) {
	contract := MustArgsContract("t", String("note").Required().NotBlank().Desc("Note."))
	if _, err := contract.Decode(`{}`); err == nil {
		t.Fatal("Required must reject an omitted argument, blank or not")
	}
	if _, err := contract.Decode(`{"note":"  "}`); err == nil {
		t.Fatal("NotBlank must reject a blank value even though it is present")
	}
	if _, err := contract.Decode(`{"note":"x"}`); err != nil {
		t.Fatalf("a present non-blank value must pass: %v", err)
	}
}

// Two pattern sources used to lose one silently: an explicit pattern replaced the shape
// the format projected, so a value that was not a date passed the contract.
func TestFormatAndExplicitPatternBothApply(t *testing.T) {
	contract := MustArgsContract("t", Date("day").Pattern(`^x`).Desc("Day."))
	property := contract.Schema().Properties["day"]
	if property.Pattern != formatPatterns["date"] || len(property.AllOf) != 1 || property.AllOf[0].Pattern != "^x" {
		t.Fatalf("schema = %+v", property)
	}
	if _, err := contract.Decode(`{"day":"xyz"}`); err == nil {
		t.Fatal("a value that is not a date must fail even though it matches the explicit pattern")
	}
	if _, err := contract.Decode(`{"day":"2026-08-17"}`); err == nil {
		t.Fatal("a date that misses the explicit pattern must fail")
	}
}

// A format and NotBlank are two constraints, so the value must satisfy both, and both
// appear in the summary the model reads when it gets the value wrong.
func TestFormatAndNotBlankBothApply(t *testing.T) {
	contract := MustArgsContract("t", Date("day").NotBlank().Desc("Day."))
	property := contract.Schema().Properties["day"]
	if property.Pattern != formatPatterns["date"] || len(property.AllOf) != 1 || property.AllOf[0].Pattern != notBlankPattern {
		t.Fatalf("schema = %+v", property)
	}
	if _, err := contract.Decode(`{"day":"   "}`); err == nil {
		t.Fatal("a blank value must fail")
	}
	if _, err := contract.Decode(`{"day":"2026-08-17"}`); err != nil {
		t.Fatalf("a date must pass: %v", err)
	}

	_, err := contract.Decode(`{"day":"17/08/2026"}`)
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatalf("error type = %T", err)
	}
	if !strings.Contains(argumentError.ExpectedArguments, "format date, non-blank") {
		t.Fatalf("summary = %s", argumentError.ExpectedArguments)
	}
}
