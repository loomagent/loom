package toolcontract

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/loomagent/loom/internal/schema"
)

func compileJSON(t *testing.T, raw string) *Validator {
	t.Helper()
	var s schema.Schema
	if err := jsonv2.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	validator, err := Compile(&s)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return validator
}

// Instance numbers keep their exact text, so integer detection must not be
// fooled by the float64 rounding that a decoded schema value would suffer.
func TestValidateKeepsExactIntegers(t *testing.T) {
	validator := compileJSON(t, `{
		"type":"object",
		"properties":{"id":{"type":"integer"}},
		"required":["id"],
		"additionalProperties":false
	}`)
	if err := validator.Validate(jsontext.Value(`{"id":9007199254740993}`)); err != nil {
		t.Fatalf("exact integer rejected: %v", err)
	}
	validationErr := validator.Validate(jsontext.Value(`{"id":1.5,"extra":true}`))
	if validationErr == nil {
		t.Fatal("invalid arguments accepted")
	}
	assertViolation(t, validationErr.Violations, "id", "type_mismatch")
	assertViolation(t, validationErr.Violations, "", "additional_property_mismatch")
}

func TestValidateReportsEveryViolation(t *testing.T) {
	validator := compileJSON(t, `{
		"type":"object",
		"properties":{
			"name":{"type":"string","minLength":2},
			"limit":{"type":"integer","minimum":1,"maximum":9}
		},
		"required":["name","limit"],
		"additionalProperties":false
	}`)
	validationErr := validator.Validate(jsontext.Value(`{"name":"a","limit":0,"extra":1}`))
	if validationErr == nil {
		t.Fatal("invalid arguments accepted")
	}
	assertViolation(t, validationErr.Violations, "name", "string_too_short")
	assertViolation(t, validationErr.Violations, "limit", "value_below_minimum")
	assertViolation(t, validationErr.Violations, "", "additional_property_mismatch")
}

func TestValidateReportsMissingRequiredAndConstMismatch(t *testing.T) {
	validator := compileJSON(t, `{
		"type":"object",
		"properties":{"kind":{"const":"a"}},
		"required":["kind"],
		"additionalProperties":false
	}`)
	validationErr := validator.Validate(jsontext.Value(`{"kind":"b"}`))
	if validationErr == nil {
		t.Fatal("invalid arguments accepted")
	}
	assertViolation(t, validationErr.Violations, "kind", "const_mismatch")

	validationErr = validator.Validate(jsontext.Value(`{}`))
	if validationErr == nil {
		t.Fatal("missing required argument accepted")
	}
	// The object pointer is empty; the property name is attached later, when the
	// violation is turned into a field diagnostic.
	assertViolation(t, validationErr.Violations, "", "missing_required_property")
}

// A keyword Loom does not model must not decode: a silently dropped
// constraint would validate as if it were absent.
func TestSchemaRejectsUnknownKeyword(t *testing.T) {
	var s schema.Schema
	if err := jsonv2.Unmarshal([]byte(`{"type":"object","$ref":"#/$defs/x"}`), &s); err == nil {
		t.Fatal("decoded a schema that uses an unsupported keyword")
	}
}

func TestViolationFieldDecodesJSONPointer(t *testing.T) {
	violation := Violation{JSONPointer: "/nested/a~1b/~0name"}
	if got, want := violation.Field(), "nested.a/b.~name"; got != want {
		t.Fatalf("Field() = %q, want %q", got, want)
	}
}

// Patterns are compiled once, at Compile time, so Validate never compiles a
// regular expression on a request path.
func TestCompileCachesPatterns(t *testing.T) {
	validator := compileJSON(t, `{
		"type":"object",
		"properties":{"q":{"type":"string","pattern":"^SRC-\\d+$"}},
		"required":["q"]
	}`)
	property := validator.schema.Properties["q"]
	if validator.patterns[property] == nil {
		t.Fatal("pattern was not compiled at Compile time")
	}
}

func BenchmarkValidatePattern(b *testing.B) {
	var s schema.Schema
	if err := jsonv2.Unmarshal([]byte(`{
		"type":"object",
		"properties":{"q":{"type":"string","pattern":"^SRC-\\d+$"}},
		"required":["q"],
		"additionalProperties":false
	}`), &s); err != nil {
		b.Fatal(err)
	}
	validator, err := Compile(&s)
	if err != nil {
		b.Fatal(err)
	}
	raw := jsontext.Value(`{"q":"SRC-123"}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := validator.Validate(raw); err != nil {
			b.Fatal(err)
		}
	}
}

// Objects compare equal regardless of key order, and numbers compare by value:
// the same value must not be judged different because it was written
// differently. Text comparison would fail both, and map key order in
// encoding/json/v2 is not even stable.
func TestEqualJSONIsOrderAndNumberInsensitive(t *testing.T) {
	for _, tc := range []struct {
		name        string
		left, right any
		equal       bool
	}{
		{"key order", map[string]any{"a": jsonNumber("1"), "b": jsonNumber("2")}, map[string]any{"b": jsonNumber("2"), "a": jsonNumber("1")}, true},
		{"number forms", jsonNumber("1.0"), jsonNumber("1"), true},
		{"exponent", jsonNumber("1e0"), jsonNumber("1"), true},
		{"nested", []any{map[string]any{"a": jsonNumber("1.0")}}, []any{map[string]any{"a": jsonNumber("1")}}, true},
		{"different value", map[string]any{"a": jsonNumber("1")}, map[string]any{"a": jsonNumber("2")}, false},
		{"array order matters", []any{jsonNumber("1"), jsonNumber("2")}, []any{jsonNumber("2"), jsonNumber("1")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := equalJSON(tc.left, tc.right); got != tc.equal {
				t.Fatalf("equalJSON = %v, want %v", got, tc.equal)
			}
		})
	}
}

func assertViolation(t *testing.T, violations []Violation, field, code string) {
	t.Helper()
	for _, violation := range violations {
		if violation.Field() == field && violation.Code == code {
			return
		}
	}
	t.Fatalf("violations = %#v, want field %q code %q", violations, field, code)
}
