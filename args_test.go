package loom

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func testWebSearchContract(t *testing.T) *ArgsContract {
	t.Helper()
	return MustArgsContract("web_search",
		String("query").Required().MinLen(1).MaxLen(40).Desc("Search query."),
		Enum("type", "search", "news").Desc("Result type."),
		Date("date_from").Desc("Optional lower bound."),
		Date("date_to").Desc("Optional upper bound."),
		ValidateArgs(func(_ context.Context, args Args) error {
			from, to := args.String("date_from"), args.String("date_to")
			if from != "" && to != "" && from > to {
				return InvalidAt("date_to", "date_to (%s) must not precede date_from (%s)", to, from)
			}
			return nil
		}),
	)
}

func TestArgsContractSchema(t *testing.T) {
	contract := testWebSearchContract(t)
	schema := contract.Schema()

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
	contract := testWebSearchContract(t)
	args, err := contract.Decode(`{"query":"go generics","type":"news","date_from":"2026-08-01","date_to":"2026-08-20"}`)
	if err != nil {
		t.Fatalf("Decode() = %v, want nil", err)
	}
	if got := args.String("query"); got != "go generics" {
		t.Fatalf("query = %q", got)
	}
	if !args.Has("type") || args.String("type") != "news" {
		t.Fatalf("type = %q, present=%v", args.String("type"), args.Has("type"))
	}
}

func TestArgsContractDecodeOptionalOmitted(t *testing.T) {
	contract := testWebSearchContract(t)
	args, err := contract.Decode(`{"query":"go"}`)
	if err != nil {
		t.Fatalf("Decode() = %v, want nil", err)
	}
	if args.Has("type") {
		t.Fatal("type should be absent")
	}
	if got := args.String("type"); got != "" {
		t.Fatalf("omitted type = %q, want empty", got)
	}
}

func TestArgsContractDecodeSchemaViolations(t *testing.T) {
	contract := testWebSearchContract(t)
	for _, raw := range []string{
		`{}`,                              // missing required
		`{"query":"go","type":"scholar"}`, // enum
		`{"query":"go","extra":1}`,        // additional property
		`{"query":"go","date_from":"nope"}`,
		`{"query":123}`, // type
	} {
		_, err := contract.Decode(raw)
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
	contract := MustArgsContract("read_reference",
		String("src_id").Required().Pattern(`^SRC-\d+$`).Validate(func(_ context.Context, value string) error {
			if value == "SRC-404" {
				return Invalid("unknown source id %q; use an id from the source index", value)
			}
			return nil
		}),
	)
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

func TestArgsContractValidatorsCollectEveryProblem(t *testing.T) {
	contract := MustArgsContract("search",
		String("from").Validate(func(_ context.Context, value string) error {
			if value == "bad" {
				return Invalid("from is bad")
			}
			return nil
		}),
		String("to").Validate(func(_ context.Context, value string) error {
			if value == "bad" {
				return errors.Join(Invalid("to is bad"), Invalid("to is also empty"))
			}
			return nil
		}),
	)
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
	contract := MustArgsContract("gate",
		String("q").Required().Validate(func(_ context.Context, _ string) error {
			ran = true
			return nil
		}),
	)
	if _, err := contract.Decode(`{}`); err == nil {
		t.Fatal("missing required argument accepted")
	}
	if ran {
		t.Fatal("field validator ran despite a schema violation")
	}
}

func TestArgsContractInternalValidatorError(t *testing.T) {
	internal := errors.New("database unavailable")
	contract := MustArgsContract("internal",
		String("q").Validate(func(_ context.Context, _ string) error { return internal }),
	)
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

func TestArgsGettersPanicOnMisuse(t *testing.T) {
	contract := MustArgsContract("t", String("q"), Int("n"))
	args, err := contract.Decode(`{"q":"x","n":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := args.Int("n"); got != 3 {
		t.Fatalf("n = %d, want 3", got)
	}
	assertPanics(t, func() { args.String("missing") })
	assertPanics(t, func() { args.Int("q") })
}

func TestNewArgsToolEndToEnd(t *testing.T) {
	contract := testWebSearchContract(t)
	tool := NewArgsTool(contract, "Search the web.", func(_ context.Context, args Args) (string, error) {
		return `"` + args.String("query") + `"`, nil
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

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}
