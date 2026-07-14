package loom

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSchemaForStruct(t *testing.T) {
	type filter struct {
		Term string `json:"term" validate:"min=2"`
	}
	type request struct {
		Query  string  `json:"query" jsonschema:"Focused search query." validate:"min=1"`
		Limit  int     `json:"limit,omitempty" jsonschema:"Maximum number of results." validate:"omitempty,min=0,max=10"`
		Filter *filter `json:"filter,omitempty"`
	}

	schema, err := SchemaFor[request]()
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	if schema.Type != "object" {
		t.Fatalf("type = %q, want object", schema.Type)
	}
	if !slices.Equal(schema.Required, []string{"query"}) {
		t.Fatalf("required = %v, want [query]", schema.Required)
	}
	if got := schema.Properties["query"].Description; got != "Focused search query." {
		t.Errorf("query description = %q", got)
	}
	if got := schema.Properties["limit"].Type; got != "integer" {
		t.Errorf("limit type = %q, want integer", got)
	}
	if got := schema.Properties["limit"].Maximum; got == nil || *got != 10 {
		t.Errorf("limit maximum = %v, want 10", got)
	}
	if got := schema.Properties["query"].MinLength; got == nil || *got != 1 {
		t.Errorf("query minLength = %v, want 1", got)
	}
	if got := schema.Properties["filter"].Properties["term"].MinLength; got == nil || *got != 2 {
		t.Errorf("filter.term minLength = %v, want 2", got)
	}
	if schema.AdditionalProperties == nil {
		t.Error("struct schema should reject additional properties")
	}
}

func TestDecodeToolArgumentsValidatesGeneratedSchema(t *testing.T) {
	type request struct {
		Query string `json:"query" validate:"min=1"`
		Limit int    `json:"limit,omitempty" validate:"omitempty,min=0,max=10"`
		Mode  string `json:"mode" validate:"oneof=fast deep"`
	}

	got, err := DecodeToolArguments[request](`{"query":"loom","limit":3,"mode":"deep"}`)
	if err != nil {
		t.Fatalf("DecodeToolArguments: %v", err)
	}
	if got.Query != "loom" || got.Limit != 3 || got.Mode != "deep" {
		t.Fatalf("decoded = %+v", got)
	}

	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{"missing required", `{"mode":"fast"}`, "required"},
		{"too large", `{"query":"loom","limit":11,"mode":"fast"}`, `"limit" must be at most 10`},
		{"bad enum", `{"query":"loom","mode":"slow"}`, `"mode" must be one of ["fast","deep"]`},
		{"unknown property", `{"query":"loom","mode":"fast","extra":true}`, `"extra" is not an accepted field`},
		{"multiple values", `{"query":"loom","mode":"fast"} {}`, "exactly one JSON object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeToolArguments[request](test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestDecodeToolArgumentsUsesGoPlaygroundValidator(t *testing.T) {
	type request struct {
		Value string `json:"value" validate:"contains=loom"`
	}
	if _, err := DecodeToolArguments[request](`{"value":"other"}`); err == nil || !strings.Contains(err.Error(), `"value" must contain "loom"`) {
		t.Fatalf("error = %v, want go-playground contains validation error", err)
	}
	if _, err := DecodeToolArguments[request](`{"value":"loom-agent"}`); err != nil {
		t.Fatalf("valid value: %v", err)
	}
}

func TestDecodeToolArgumentsReturnsInvalidValidatorTag(t *testing.T) {
	type request struct {
		Value string `json:"value" validate:"not_a_real_rule"`
	}
	_, err := DecodeToolArguments[request](`{"value":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "validation is misconfigured") {
		t.Fatalf("error = %v", err)
	}
	if unwrapped := errors.Unwrap(err); unwrapped == nil || !strings.Contains(unwrapped.Error(), "invalid validator tag") {
		t.Fatalf("unwrapped error = %v", unwrapped)
	}
}

func TestDecodeToolArgumentsForIncludesToolAndExpectedArguments(t *testing.T) {
	type request struct {
		Query string `json:"query" validate:"min=1"`
		Limit int    `json:"limit,omitempty" validate:"omitempty,min=0,max=10"`
	}

	_, err := DecodeToolArgumentsFor[request]("web_search", `{"limit":1}`)
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatalf("error type = %T, want *ToolArgumentError", err)
	}
	if argumentError.Tool != "web_search" || argumentError.Kind != ToolArgumentErrorSchema {
		t.Fatalf("error metadata = %+v", argumentError)
	}
	if got, want := argumentError.ExpectedArguments, `query=<string, required, min length 1>; limit=<integer, optional, 0..10>`; got != want {
		t.Fatalf("expected arguments = %s, want %s", got, want)
	}
	if got := err.Error(); !strings.Contains(got, `invalid arguments for tool "web_search"`) || !strings.Contains(got, `"query" is required`) {
		t.Fatalf("error = %s", got)
	}
}

func TestDecodeToolArgumentsForFormatsNonBlankRule(t *testing.T) {
	type request struct {
		Query string `json:"query" validate:"min=1,notblank"`
	}

	_, err := DecodeToolArgumentsFor[request]("web_search", `{"query":"   "}`)
	if err == nil || !strings.Contains(err.Error(), `"query" must not be blank`) {
		t.Fatalf("error = %v", err)
	}
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) || !strings.Contains(argumentError.ExpectedArguments, "non-blank") {
		t.Fatalf("argument error = %+v", argumentError)
	}
}

func TestSchemaForProjectsExamples(t *testing.T) {
	type request struct {
		Query   string   `json:"query" validate:"min=1,notblank" example:"loom agent runtime"`
		Limit   int      `json:"limit,omitempty" validate:"omitempty,min=1,max=10" example:"5"`
		Enabled bool     `json:"enabled,omitempty" example:"true"`
		Tags    []string `json:"tags,omitempty" example:"[\"go\",\"agents\"]"`
	}

	schema, err := SchemaFor[request]()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := schema.Properties["query"].Examples, []any{"loom agent runtime"}; !slices.Equal(got, want) {
		t.Fatalf("query examples = %#v, want %#v", got, want)
	}
	if got := schema.Properties["limit"].Examples; len(got) != 1 || got[0] != int64(5) {
		t.Fatalf("limit examples = %#v", got)
	}
	if got := schema.Properties["tags"].Examples; len(got) != 1 {
		t.Fatalf("tags examples = %#v", got)
	}
}

func TestMustSchemaForReturnsIndependentSchemas(t *testing.T) {
	type request struct {
		Value string `json:"value"`
	}

	first := MustSchemaFor[request]()
	first.Properties["value"].Description = "changed"
	second := MustSchemaFor[request]()
	if got := second.Properties["value"].Description; got != "" {
		t.Fatalf("schema mutation leaked between calls: %q", got)
	}
}
