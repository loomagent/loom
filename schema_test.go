package loom

import (
	"slices"
	"testing"
)

// SchemaFor remains for structured model output, where the model returns JSON
// that is decoded into a Go value. Tool arguments use ArgsContract instead.
func TestSchemaForProjectsStructShape(t *testing.T) {
	type request struct {
		Query string   `json:"query" jsonschema:"Search query."`
		Limit int      `json:"limit,omitempty" jsonschema:"Maximum results." validate:"omitempty,min=0,max=10"`
		Mode  string   `json:"mode" validate:"oneof=fast deep"`
		When  string   `json:"when,omitempty" validate:"omitempty,datetime=2006-01-02"`
		Tags  []string `json:"tags,omitempty" validate:"omitempty,unique"`
	}
	schema, err := SchemaFor[request]()
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	if got := schema.Properties["query"].Description; got != "Search query." {
		t.Errorf("query description = %q", got)
	}
	if !slices.Contains(schema.Required, "query") {
		t.Errorf("query should be required: %v", schema.Required)
	}
	if slices.Contains(schema.Required, "limit") {
		t.Errorf("limit is omitempty and must be optional: %v", schema.Required)
	}
	if got, want := schema.Properties["mode"].Enum, []any{"fast", "deep"}; !slices.Equal(got, want) {
		t.Errorf("mode enum = %v, want %v", got, want)
	}
	if got := schema.Properties["when"].Format; got != "date" {
		t.Errorf("when format = %q, want date for a date-only layout", got)
	}
	if !schema.Properties["tags"].UniqueItems {
		t.Error("tags uniqueItems was not projected")
	}
}

func TestSchemaForProjectsExamples(t *testing.T) {
	type request struct {
		Expression string `json:"expression" example:"(2 + 3) * 4"`
	}
	schema, err := SchemaFor[request]()
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	if got := schema.Properties["expression"].Examples; len(got) != 1 || got[0] != "(2 + 3) * 4" {
		t.Errorf("example = %v", got)
	}
}

// An omitempty field must not end up with a schema stricter than the validator
// that actually decides: the empty value has to satisfy every projected rule.
func TestSchemaForExemptsEmptyValuesOfOptionalFields(t *testing.T) {
	type request struct {
		Q     string   `json:"q,omitempty" validate:"omitempty,min=3"`
		Mode  string   `json:"mode,omitempty" validate:"omitempty,oneof=fast slow"`
		Limit int      `json:"limit,omitempty" validate:"omitempty,max=10"`
		Tags  []string `json:"tags,omitempty" validate:"omitempty,unique"`
	}
	schema, err := SchemaFor[request]()
	if err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties["q"].MinLength; got != nil {
		t.Errorf("q minLength = %v, want unset", *got)
	}
	if got := schema.Properties["mode"].Enum; got != nil {
		t.Errorf("mode enum = %v, want unset", got)
	}
	if got := schema.Properties["limit"].Maximum; got == nil || *got != 10 {
		t.Errorf("limit maximum = %v, want 10 kept", got)
	}
	if !schema.Properties["tags"].UniqueItems {
		t.Error("tags uniqueItems dropped, but an empty array satisfies it")
	}
}

func TestMustSchemaForReturnsIndependentSchemas(t *testing.T) {
	type request struct {
		Value string `json:"value"`
	}
	first := MustSchemaFor[request]()
	second := MustSchemaFor[request]()
	first.Properties["value"].Description = "mutated"
	if second.Properties["value"].Description == "mutated" {
		t.Fatal("MustSchemaFor returned a shared schema")
	}
}
