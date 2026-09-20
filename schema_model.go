package loom

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"

	"github.com/loomagent/loom/internal/schema"
)

// Schema is the JSON Schema model Loom builds and validates against. See the
// internal/schema package for the definition; it is aliased here so callers can
// build and inspect schemas without importing an internal package.
type Schema = schema.Schema

// boolPtr returns a pointer to v, for the boolean schema keywords.
func boolPtr(v bool) *bool { return &v }

// ConstJSON renders value as the raw JSON that a schema's const keyword holds.
// A const is raw JSON rather than a Go value so a literal null is distinct from
// no const at all, and a large integer keeps its exact digits.
func ConstJSON(value any) jsontext.Value {
	data, err := jsonv2.Marshal(value)
	if err != nil {
		// The callers pass bools and strings; a type that cannot be marshaled is
		// a programming error in the schema being built.
		panic(fmt.Sprintf("loom: const %T cannot be marshaled: %v", value, err))
	}
	return data
}
