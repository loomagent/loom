package toolcontract

import (
	"encoding/json/jsontext"
	"testing"
)

func TestValidatePreservesExactNumbersAndStructuredViolations(t *testing.T) {
	validator, err := Compile([]byte(`{
		"type":"object",
		"properties":{"id":{"const":9007199254740993}},
		"required":["id"],
		"additionalProperties":false
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.Validate(jsontext.Value(`{"id":9007199254740993}`)); err != nil {
		t.Fatalf("exact integer rejected: %v", err)
	}
	validationErr := validator.Validate(jsontext.Value(`{"id":9007199254740992,"extra":true}`))
	if validationErr == nil {
		t.Fatal("invalid arguments accepted")
	}
	assertViolation(t, validationErr.Violations, "id", "const_mismatch")
	assertViolation(t, validationErr.Violations, "", "additional_property_mismatch")
}

func TestViolationFieldDecodesJSONPointer(t *testing.T) {
	violation := Violation{JSONPointer: "/nested/a~1b/~0name"}
	if got, want := violation.Field(), "nested.a/b.~name"; got != want {
		t.Fatalf("Field() = %q, want %q", got, want)
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
