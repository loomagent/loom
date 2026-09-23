// Package schema defines the JSON Schema model Loom builds, sends to providers,
// and validates against. It carries only the keywords Loom's declared-argument
// builder emits, which keeps the model — and the validator that walks it — a
// closed, testable subset instead of a general JSON Schema implementation.
//
// The package lives under internal/ so both the loom package and the validator
// can share one definition without an import cycle.
package schema

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"sort"
)

// Schema is a JSON Schema document or subschema. A schema is a plain value:
// build it as a struct literal and marshal it with encoding/json/v2. The zero
// value is an empty schema, which accepts anything.
//
// Decoding rejects keywords outside this set rather than dropping them, so a
// schema that carries something Loom cannot enforce fails loudly instead of
// validating as if the constraint were absent.
type Schema struct {
	// external is a caller-validated schema transported without interpreting keywords.
	external jsontext.Value

	// Identity and dialect.
	ID     string `json:"$id,omitempty"`
	Schema string `json:"$schema,omitempty"`

	// Type is a single type name. Loom never emits a type union, so there is no
	// Types field; every schema it builds has at most one type.
	Type string `json:"type,omitempty"`

	// Object keywords.
	Properties map[string]*Schema `json:"properties,omitempty"`
	// PropertyOrder preserves the author's declaration order for diagnostics.
	// It is not part of the JSON representation.
	PropertyOrder []string `json:"-"`
	Required      []string `json:"required,omitempty"`
	// AdditionalProperties is a boolean. Loom sets false; nil means unset, so
	// the keyword is omitted entirely.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`

	// Array keywords.
	Items *Schema `json:"items,omitempty"`

	// AllOf holds subschemas the instance must satisfy in full. It is how the subset
	// says "this value obeys all of these", which a single value keyword cannot: a
	// property has one pattern, so an argument that is both a date and non-blank needs
	// two branches rather than one keyword that replaces the other.
	AllOf []*Schema `json:"allOf,omitempty"`

	// Value keywords.
	Enum []any `json:"enum,omitempty"`
	// Const is raw JSON, not a Go value: nil means "no const", while a literal
	// null is the four bytes null. A Go value could not tell those apart, and
	// would also round a large integer through float64. omitzero, not omitempty,
	// so a literal null is still emitted.
	Const            jsontext.Value `json:"const,omitzero"`
	Format           string         `json:"format,omitempty"`
	Pattern          string         `json:"pattern,omitempty"`
	Minimum          *float64       `json:"minimum,omitempty"`
	Maximum          *float64       `json:"maximum,omitempty"`
	ExclusiveMinimum *float64       `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum *float64       `json:"exclusiveMaximum,omitempty"`
	MinLength        *int           `json:"minLength,omitempty"`
	MaxLength        *int           `json:"maxLength,omitempty"`
	MinItems         *int           `json:"minItems,omitempty"`
	MaxItems         *int           `json:"maxItems,omitempty"`
	UniqueItems      bool           `json:"uniqueItems,omitzero"`
	Description      string         `json:"description,omitempty"`
	Examples         []any          `json:"examples,omitempty"`
}

// UnmarshalJSON decodes a schema, rejecting any keyword outside the model. A
// silently dropped constraint would validate as if it were absent, so an
// unsupported keyword is an error rather than a no-op.
func (s *Schema) UnmarshalJSON(data []byte) error {
	type plain Schema
	var decoded plain
	if err := jsonv2.Unmarshal(data, &decoded, jsonv2.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*s = Schema(decoded)
	return nil
}

// PropertyNames returns the schema's property names in declaration order, then
// alphabetically for anything without a recorded order.
func (s *Schema) PropertyNames() []string {
	if s == nil {
		return nil
	}
	names := append([]string(nil), s.PropertyOrder...)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	var remaining []string
	for name := range s.Properties {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(names, remaining...)
}

// External accepts a full caller-validated JSON Schema for provider transport.
// The built-in subset validator deliberately refuses these schemas.
func External(data jsontext.Value) (*Schema, error) {
	var object map[string]jsontext.Value
	if err := jsonv2.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("external schema must be a JSON object")
	}
	return &Schema{external: append(jsontext.Value(nil), data...)}, nil
}

// IsExternal identifies schemas whose semantic validation belongs to the host.
func (s *Schema) IsExternal() bool { return len(s.external) > 0 }

// MarshalJSON preserves external schema keywords and exact JSON numbers.
func (s Schema) MarshalJSON() ([]byte, error) {
	if s.IsExternal() {
		return append([]byte(nil), s.external...), nil
	}
	type plain Schema
	return jsonv2.Marshal(plain(s))
}
