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

func TestSchemaForProjectsCommonValidatorRules(t *testing.T) {
	type request struct {
		Name   string            `json:"name" validate:"required,gt=2,lt=8,startswith=lo,endswith=om,contains=oo,excludes=x"`
		Score  float64           `json:"score" validate:"gt=0,lte=1"`
		Mode   string            `json:"mode" validate:"oneof='fast mode' deep"`
		Exact  string            `json:"exact" validate:"eq=loom"`
		Other  string            `json:"other" validate:"ne=bad"`
		Email  string            `json:"email" validate:"email"`
		Tags   []string          `json:"tags,omitempty" validate:"omitempty,min=1,unique,dive,required,min=2"`
		Labels map[string]string `json:"labels,omitempty" validate:"omitempty,dive,keys,startswith=x,endkeys,required,min=2"`
	}

	schema, err := SchemaFor[request]()
	if err != nil {
		t.Fatal(err)
	}
	name := schema.Properties["name"]
	if name.MinLength == nil || *name.MinLength != 3 || name.MaxLength == nil || *name.MaxLength != 7 {
		t.Fatalf("name length schema = min %v max %v", name.MinLength, name.MaxLength)
	}
	if name.Pattern != "^lo" || len(name.AllOf) != 2 || name.Not == nil || name.Not.Pattern != "x" {
		t.Fatalf("name pattern schema = %#v", name)
	}
	score := schema.Properties["score"]
	if score.ExclusiveMinimum == nil || *score.ExclusiveMinimum != 0 || score.Maximum == nil || *score.Maximum != 1 {
		t.Fatalf("score bounds = exclusive min %v max %v", score.ExclusiveMinimum, score.Maximum)
	}
	if got, want := schema.Properties["mode"].Enum, []any{"fast mode", "deep"}; !slices.Equal(got, want) {
		t.Fatalf("mode enum = %#v, want %#v", got, want)
	}
	if exact := schema.Properties["exact"].Const; exact == nil || *exact != "loom" {
		t.Fatalf("exact const = %#v", exact)
	}
	if other := schema.Properties["other"].Not; other == nil || other.Const == nil || *other.Const != "bad" {
		t.Fatalf("other not = %#v", other)
	}
	if format := schema.Properties["email"].Format; format != "email" {
		t.Fatalf("email format = %q", format)
	}
	tags := schema.Properties["tags"]
	if !tags.UniqueItems || tags.Items.MinLength == nil || *tags.Items.MinLength != 2 {
		t.Fatalf("tags schema = %#v", tags)
	}
	if slices.Contains(schema.Required, "tags") {
		t.Fatal("required after dive must not make the optional tags property required")
	}
	labels := schema.Properties["labels"]
	if labels.PropertyNames == nil || labels.PropertyNames.Pattern != "^x" || labels.AdditionalProperties.MinLength == nil || *labels.AdditionalProperties.MinLength != 2 {
		t.Fatalf("labels schema = %#v", labels)
	}
	if _, err := DecodeToolArguments[request](`{"name":"looom","score":0.5,"mode":"fast mode","exact":"loom","other":"good","email":"agent@example.com","tags":["go","ai"],"labels":{"xkey":"ok"}}`); err != nil {
		t.Fatalf("valid projected rules: %v", err)
	}
}

func TestProjectedValidatorRulesReturnFriendlyErrors(t *testing.T) {
	type request struct {
		Name  string   `json:"name" validate:"gt=2,startswith=lo"`
		Score int      `json:"score" validate:"gt=0"`
		Tags  []string `json:"tags" validate:"unique"`
	}

	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{"starts with", `{"name":"foo","score":1,"tags":["a"]}`, `"name" must start with "lo"`},
		{"greater than", `{"name":"loom","score":0,"tags":["a"]}`, `"score" must be greater than 0`},
		{"unique", `{"name":"loom","score":1,"tags":["a","a"]}`, `"tags" must contain unique items`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeToolArguments[request](test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSchemaForProjectsValidatorOrAndAnnotatesFormats(t *testing.T) {
	type request struct {
		Level int    `json:"level" validate:"eq=1|eq=3"`
		Email string `json:"email" validate:"email"`
	}

	schema, err := SchemaFor[request]()
	if err != nil {
		t.Fatal(err)
	}
	level := schema.Properties["level"]
	if len(level.AllOf) != 1 || len(level.AllOf[0].AnyOf) != 2 {
		t.Fatalf("level alternatives = %#v", level.AllOf)
	}
	if schema.Properties["email"].Format != "email" {
		t.Fatalf("email format = %q", schema.Properties["email"].Format)
	}
	if _, err := DecodeToolArguments[request](`{"level":3,"email":"agent@example.com"}`); err != nil {
		t.Fatalf("valid OR and email: %v", err)
	}
	if _, err := DecodeToolArguments[request](`{"level":2,"email":"agent@example.com"}`); err == nil || !strings.Contains(err.Error(), `"level" must be one of [1,3]`) {
		t.Fatalf("OR error = %v", err)
	}
	if _, err := DecodeToolArguments[request](`{"level":1,"email":"not-an-email"}`); err == nil || !strings.Contains(err.Error(), `"email" must be a valid email`) {
		t.Fatalf("email error = %v", err)
	}
}

func TestDecodeToolArgumentsPreservesLargeIntegerPrecision(t *testing.T) {
	const id = uint64(9007199254740993) // First integer not exactly representable by float64.
	type request struct {
		ID       uint64   `json:"id"`
		Unsigned uint64   `json:"unsigned"`
		Signed   int64    `json:"signed"`
		Values   []uint64 `json:"values" validate:"unique"`
	}

	schema := MustSchemaFor[request]()
	expectedID := any(id)
	schema.Properties["id"].Const = &expectedID
	raw := `{"id":9007199254740993,"unsigned":18446744073709551615,"signed":-9223372036854775808,"values":[9007199254740992,9007199254740993]}`
	got, err := DecodeToolArgumentsWithSchema[request](raw, schema)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Unsigned != ^uint64(0) || got.Signed != -1<<63 {
		t.Fatalf("decoded integers = %+v", got)
	}
	if !slices.Equal(got.Values, []uint64{9007199254740992, 9007199254740993}) {
		t.Fatalf("decoded values = %v", got.Values)
	}

	wrong := strings.Replace(raw, `"id":9007199254740993`, `"id":9007199254740992`, 1)
	if _, err := DecodeToolArgumentsWithSchema[request](wrong, schema); err == nil || !strings.Contains(err.Error(), `"id" must equal 9007199254740993`) {
		t.Fatalf("adjacent integer error = %v", err)
	}
	tooLarge := strings.Replace(raw, `"unsigned":18446744073709551615`, `"unsigned":18446744073709551616`, 1)
	if _, err := DecodeToolArgumentsWithSchema[request](tooLarge, schema); err == nil || !strings.Contains(err.Error(), "outside the supported 64-bit integer range") {
		t.Fatalf("out-of-range integer error = %v", err)
	}
}

func TestDecodeToolArgumentsNormalizesIntegralExponent(t *testing.T) {
	type request struct {
		Value int64 `json:"value"`
	}
	got, err := DecodeToolArguments[request](`{"value":1e3}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != 1000 {
		t.Fatalf("value = %d, want 1000", got.Value)
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
