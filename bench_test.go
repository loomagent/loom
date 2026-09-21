package loom

import (
	"testing"

	jsonv2 "encoding/json/v2"
)

// The contract decode and the handle reads sit on the path of every tool call, so their
// cost is worth a number. In particular the declared-name check on Get and Present has to
// stay cheaper than the work it guards.

func BenchmarkArgsContractDecode(b *testing.B) {
	contract := MustArgsContract("web_search",
		String("query").Required().MinLen(1).MaxLen(200).Desc("Query."),
		Enum("type", "search", "news").Desc("Type."),
		Date("date_from").Desc("Lower bound."),
		Date("date_to").Desc("Upper bound."),
		Uint("limit").Max(20).Desc("Limit."),
	)
	raw := `{"query":"golang generics","type":"search","date_from":"2026-08-17","date_to":"2026-08-18","limit":10}`
	b.ReportAllocs()
	for b.Loop() {
		if _, err := contract.Decode(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkArgRead(b *testing.B) {
	query := String("query").Required().Desc("Query.")
	limit := Uint("limit").Desc("Limit.")
	contract := MustArgsContract("t", query, limit)
	args, err := contract.Decode(`{"query":"golang generics","limit":10}`)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if query.Get(args) == "" || !query.Present(args) || limit.Get(args) != 10 {
			b.Fatal("read failed")
		}
	}
}

// ValidateSchema compiles the schema on every call, so these three benchmarks separate the
// costs a caller pays: compiling, marshaling the value, and walking it. The middle one is
// what a cache would remove; the last is the floor underneath it.

func benchSchema() *Schema {
	minimum, maximum := 0.0, 20.0
	minLength, maxLength, minItems := 1, 200, 1
	strict := false
	return &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"query": {Type: "string", MinLength: &minLength, MaxLength: &maxLength, Pattern: notBlankPattern},
			"mode":  {Type: "string", Enum: []any{"fast", "slow"}},
			"day":   {Type: "string", Format: "date", Pattern: formatPatterns["date"]},
			"limit": {Type: "integer", Minimum: &minimum, Maximum: &maximum},
			"tags":  {Type: "array", Items: &Schema{Type: "string"}, MinItems: &minItems, UniqueItems: true},
		},
		Required:             []string{"query"},
		AdditionalProperties: &strict,
	}
}

func benchValue() map[string]any {
	return map[string]any{
		"query": "golang generics",
		"mode":  "fast",
		"day":   "2026-08-17",
		"limit": 10,
		"tags":  []any{"a", "b"},
	}
}

func BenchmarkValidateSchema(b *testing.B) {
	schema := benchSchema()
	b.ReportAllocs()
	for b.Loop() {
		if err := ValidateSchema(schema, benchValue()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompileValidationSchema(b *testing.B) {
	schema := benchSchema()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := compileValidationSchema(schema); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidatorValidate(b *testing.B) {
	validator, err := compileValidationSchema(benchSchema())
	if err != nil {
		b.Fatal(err)
	}
	raw, err := jsonv2.Marshal(benchValue())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if failure := validator.Validate(raw); failure != nil {
			b.Fatal(failure)
		}
	}
}

// The third cost inside ValidateSchema, so the split is measured rather than inferred.
func BenchmarkValidateSchemaMarshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := jsonv2.Marshal(benchValue()); err != nil {
			b.Fatal(err)
		}
	}
}
