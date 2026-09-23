package loom

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"testing"
)

func TestUserTransportsBusinessSchemaWithoutLosingConstraints(t *testing.T) {
	// Given a host-owned schema beyond the declared-argument subset.
	raw := jsontext.Value(`{"type":"object","$defs":{"id":{"type":"integer","maximum":18446744073709551615}},"properties":{"id":{"$ref":"#/$defs/id"}},"additionalProperties":false}`)
	schema, err := ExternalSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	// When sent to a provider, preserve every constraint and exact integer.
	encoded, err := jsonv2.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(raw) {
		t.Fatalf("schema changed: %s", encoded)
	}
	raw[0] = '['
	encoded, err = jsonv2.Marshal(schema)
	if err != nil || encoded[0] != '{' {
		t.Fatalf("caller mutated schema: %s %v", encoded, err)
	}
	// Then the subset validator must never pretend to validate this contract.
	if err := ValidateSchema(schema, map[string]any{"id": 1}); err == nil {
		t.Fatal("external contract accepted by subset validator")
	}
	if err := ValidateSchema(&Schema{Type: "object", Properties: map[string]*Schema{"nested": schema}}, map[string]any{}); err == nil {
		t.Fatal("nested external contract accepted")
	}
	for _, invalid := range []string{`null`, `[]`, `{"type":"object","type":"string"}`, `{"x":"` + string([]byte{0xff}) + `"}`} {
		if _, err := ExternalSchema(jsontext.Value(invalid)); err == nil {
			t.Fatalf("invalid schema accepted: %q", invalid)
		}
	}
}
