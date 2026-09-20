package toolcontract

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"strings"
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

func TestCompileRejectsUnsupportedKeyword(t *testing.T) {
	_, err := Compile(&schema.Schema{Ref: "#/$defs/x"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Compile() = %v, want an unsupported-keyword error", err)
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

func assertViolation(t *testing.T, violations []Violation, field, code string) {
	t.Helper()
	for _, violation := range violations {
		if violation.Field() == field && violation.Code == code {
			return
		}
	}
	t.Fatalf("violations = %#v, want field %q code %q", violations, field, code)
}
