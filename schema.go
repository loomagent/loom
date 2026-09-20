package loom

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/loomagent/loom/internal/toolcontract"
)

var errMultipleJSONValues = errors.New("multiple JSON values")

// readStrictJSON reads exactly one JSON value. Tool arguments and structured
// output both require the whole payload to be one value, so trailing content is
// an error rather than something to trim.
func readStrictJSON(input string) (jsontext.Value, error) {
	decoder := jsontext.NewDecoder(strings.NewReader(input))
	raw, err := decoder.ReadValue()
	if err != nil {
		return nil, err
	}
	raw = raw.Clone()
	if _, err := decoder.ReadValue(); err == nil {
		return nil, errMultipleJSONValues
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return raw, nil
}

func compileValidationSchema(schema *Schema) (*toolcontract.Validator, error) {
	data, err := jsonv2.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return toolcontract.Compile(data)
}

// ValidateSchema reports whether value satisfies schema. It is the check Loom
// applies to tool arguments, structured output, and declared examples.
func ValidateSchema(schema *Schema, value any) error {
	if schema == nil {
		return nil
	}
	validator, err := compileValidationSchema(schema)
	if err != nil {
		return err
	}
	data, err := jsonv2.Marshal(value)
	if err != nil {
		return err
	}
	if failure := validator.Validate(jsontext.Value(data)); failure != nil {
		return failure
	}
	return nil
}

// cloneSchema deep-copies a schema. A contract hands its schema to callers that
// may mutate it, and those writes must not reach the schema the contract
// validates against, nor race with concurrent calls.
func cloneSchema(schema *Schema) *Schema {
	if schema == nil {
		return nil
	}
	clone := *schema
	clone.PropertyOrder = slices.Clone(schema.PropertyOrder)
	clone.Required = slices.Clone(schema.Required)
	clone.Enum = slices.Clone(schema.Enum)
	clone.Examples = slices.Clone(schema.Examples)
	clone.Not = cloneSchema(schema.Not)
	clone.Items = cloneSchema(schema.Items)
	clone.AllOf = cloneSchemaList(schema.AllOf)
	clone.AnyOf = cloneSchemaList(schema.AnyOf)
	clone.OneOf = cloneSchemaList(schema.OneOf)
	if schema.Defs != nil {
		clone.Defs = make(map[string]*Schema, len(schema.Defs))
		for name, def := range schema.Defs {
			clone.Defs[name] = cloneSchema(def)
		}
	}
	if schema.Properties != nil {
		clone.Properties = make(map[string]*Schema, len(schema.Properties))
		for name, property := range schema.Properties {
			clone.Properties[name] = cloneSchema(property)
		}
	}
	return &clone
}

func cloneSchemaList(schemas []*Schema) []*Schema {
	if schemas == nil {
		return nil
	}
	out := make([]*Schema, len(schemas))
	for i, schema := range schemas {
		out[i] = cloneSchema(schema)
	}
	return out
}

func schemaHasType(schema *Schema, want string) bool {
	return schema != nil && schema.Type == want
}
