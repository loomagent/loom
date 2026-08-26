// Package toolcontract compiles and validates the JSON Schema subset used by
// Loom tool arguments. It isolates the general-purpose schema engine from the
// stable, LLM-facing violation protocol consumed by the loom package.
package toolcontract

import (
	"encoding/json/jsontext"
	"strings"

	jsonschema "github.com/kaptinlin/jsonschema"
)

// Validator is an immutable compiled tool-argument contract.
type Validator struct {
	schema *jsonschema.Schema
}

// Violation is one machine-readable tool-contract failure.
type Violation struct {
	JSONPointer string
	Keyword     string
	Code        string
	Params      map[string]any
}

// ValidationError reports all schema violations found in one tool call.
type ValidationError struct {
	Violations []Violation
}

func (e *ValidationError) Error() string { return "tool arguments do not match the contract" }

// Compile builds an immutable validator from a JSON Schema document.
func Compile(schemaJSON []byte) (*Validator, error) {
	schema, err := jsonschema.NewCompiler().Compile(schemaJSON)
	if err != nil {
		return nil, err
	}
	return &Validator{schema: schema}, nil
}

// Validate checks one already syntax-validated JSON value.
func (v *Validator) Validate(raw jsontext.Value) *ValidationError {
	result := v.schema.ValidateJSON(raw)
	if result.IsValid() {
		return nil
	}
	validationError := &ValidationError{}
	collectViolations(result, "", &validationError.Violations)
	return validationError
}

func collectViolations(result *jsonschema.EvaluationResult, parentPointer string, violations *[]Violation) {
	pointer := result.InstanceLocation
	if pointer == "" {
		pointer = parentPointer
	}
	for _, validationError := range result.Errors {
		*violations = append(*violations, Violation{
			JSONPointer: pointer,
			Keyword:     validationError.Keyword,
			Code:        validationError.Code,
			Params:      validationError.Params,
		})
	}
	for _, detail := range result.Details {
		collectViolations(detail, pointer, violations)
	}
}

// Field converts a violation's RFC 6901 JSON Pointer to Loom's dotted field
// notation. Escaped member names are decoded before joining.
func (v Violation) Field() string {
	pointer := strings.TrimPrefix(v.JSONPointer, "/")
	if pointer == "" {
		return ""
	}
	parts := strings.Split(pointer, "/")
	for index := range parts {
		parts[index] = strings.ReplaceAll(strings.ReplaceAll(parts[index], "~1", "/"), "~0", "~")
	}
	return strings.Join(parts, ".")
}
