package schema

import (
	jsonv2 "encoding/json/v2"
	"reflect"
	"slices"
	"testing"
)

func decodeSchema(t *testing.T, raw string) Schema {
	t.Helper()
	var s Schema
	if err := jsonv2.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return s
}

// A keyword outside the model must not decode. A silently dropped constraint
// would validate as if it were absent, which is worse than refusing the schema.
func TestUnmarshalRejectsUnknownKeyword(t *testing.T) {
	for _, raw := range []string{
		`{"$ref":"#/$defs/x"}`,
		`{"$defs":{"x":{"type":"string"}}}`,
		`{"minProperties":1}`,
		`{"not":{"type":"string"}}`,
		`{"oneOf":[{"type":"string"}]}`,
		`{"properties":{"x":{"patternProperties":{"a":{"type":"string"}}}}}`,
	} {
		var s Schema
		if err := jsonv2.Unmarshal([]byte(raw), &s); err == nil {
			t.Errorf("decoded %s, want an unknown-keyword error", raw)
		}
	}
}

// Shapes the model deliberately does not carry — schema-valued
// additionalProperties, tuple items, a type union — are refused rather than
// approximated.
func TestUnmarshalRejectsUnsupportedShapes(t *testing.T) {
	for _, raw := range []string{
		`{"additionalProperties":{"type":"string"}}`,
		`{"items":[{"type":"string"}]}`,
		`{"type":["string","null"]}`,
		`{"properties":"not-an-object"}`,
	} {
		var s Schema
		if err := jsonv2.Unmarshal([]byte(raw), &s); err == nil {
			t.Errorf("decoded %s, want a type error", raw)
		}
	}
}

func TestUnmarshalDecodesModelledKeywords(t *testing.T) {
	s := decodeSchema(t, `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id": "https://example.test/s",
		"type": "object",
		"properties": {
			"a": {
				"type": "string", "minLength": 1, "maxLength": 5, "pattern": "^a",
				"enum": ["a"], "format": "date", "description": "first", "examples": ["a"]
			},
			"n": {"type": "integer", "minimum": 1, "maximum": 9, "exclusiveMinimum": 0, "exclusiveMaximum": 10}
		},
		"required": ["a"],
		"additionalProperties": false,
		"uniqueItems": true
	}`)

	if s.Schema != "https://json-schema.org/draft/2020-12/schema" || s.ID != "https://example.test/s" || s.Type != "object" {
		t.Fatalf("identity = %+v", s)
	}
	if !slices.Equal(s.Required, []string{"a"}) {
		t.Fatalf("required = %v", s.Required)
	}
	if s.AdditionalProperties == nil || *s.AdditionalProperties {
		t.Fatalf("additionalProperties = %v", s.AdditionalProperties)
	}
	if !s.UniqueItems {
		t.Fatal("uniqueItems lost")
	}

	a := s.Properties["a"]
	if a == nil {
		t.Fatal("property a lost")
	}
	if a.MinLength == nil || *a.MinLength != 1 || a.MaxLength == nil || *a.MaxLength != 5 {
		t.Fatalf("string lengths = %+v", a)
	}
	if a.Pattern != "^a" || a.Format != "date" || a.Description != "first" {
		t.Fatalf("string annotations = %+v", a)
	}
	if !slices.Equal(a.Enum, []any{"a"}) || !slices.Equal(a.Examples, []any{"a"}) {
		t.Fatalf("enum/examples = %+v", a)
	}

	n := s.Properties["n"]
	if n.Minimum == nil || *n.Minimum != 1 || n.Maximum == nil || *n.Maximum != 9 {
		t.Fatalf("bounds = %+v", n)
	}
	if n.ExclusiveMinimum == nil || *n.ExclusiveMinimum != 0 || n.ExclusiveMaximum == nil || *n.ExclusiveMaximum != 10 {
		t.Fatalf("exclusive bounds = %+v", n)
	}
}

// A const is raw JSON: a literal null is distinct from no const at all, and a
// large integer keeps its exact digits. The field uses omitzero, not omitempty,
// so a null const is still emitted.
func TestConstIsRawJSON(t *testing.T) {
	absent := decodeSchema(t, `{}`)
	if absent.Const != nil {
		t.Fatalf("absent const = %q", absent.Const)
	}
	null := decodeSchema(t, `{"const":null}`)
	if null.Const == nil || string(null.Const) != "null" {
		t.Fatalf("null const = %q", null.Const)
	}
	big := decodeSchema(t, `{"const":9007199254740993}`)
	if string(big.Const) != "9007199254740993" {
		t.Fatalf("large const = %q", big.Const)
	}

	data, err := jsonv2.Marshal(absent)
	if err != nil || string(data) != "{}" {
		t.Fatalf("absent marshaled to %s (%v)", data, err)
	}
	data, err = jsonv2.Marshal(null)
	if err != nil || string(data) != `{"const":null}` {
		t.Fatalf("null const marshaled to %s (%v)", data, err)
	}
}

func TestPropertyNames(t *testing.T) {
	s := Schema{
		PropertyOrder: []string{"second", "first"},
		Properties: map[string]*Schema{
			"first": {}, "second": {}, "third": {}, "alpha": {},
		},
	}
	if got, want := s.PropertyNames(), []string{"second", "first", "alpha", "third"}; !slices.Equal(got, want) {
		t.Fatalf("PropertyNames() = %v, want %v", got, want)
	}

	var absent *Schema
	if absent.PropertyNames() != nil {
		t.Fatalf("nil schema = %v", absent.PropertyNames())
	}
	if got := (&Schema{}).PropertyNames(); len(got) != 0 {
		t.Fatalf("schema without properties = %v", got)
	}
}

// Everything the model carries survives a marshal/decode round-trip, so a schema
// handed to a provider and the schema the engine validates are the same.
func TestMarshalRoundTrip(t *testing.T) {
	original := decodeSchema(t, `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {
			"a": {"type": "string", "enum": ["a"], "pattern": "^a", "const": null},
			"b": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 3, "uniqueItems": true}
		},
		"required": ["a"],
		"additionalProperties": false
	}`)
	data, err := jsonv2.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var round Schema
	if err := jsonv2.Unmarshal(data, &round); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	if !reflect.DeepEqual(original, round) {
		t.Fatalf("round-trip changed the schema:\n original = %+v\n round    = %+v", original, round)
	}
}
