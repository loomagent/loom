package loom

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

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

func compileValidationSchema(schema *jsonschema.Schema) (*toolcontract.Validator, error) {
	data, err := jsonv2.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return toolcontract.Compile(data)
}

// cloneSchema deep-copies a schema.
//
// jsonschema.Schema.CloneSchemas only clones nested *Schema values; slices and
// pointers holding plain values — Required, Enum, Examples, Minimum, MaxLength
// and friends — stay shared with the original. That is not enough here: a
// contract hands its schema to callers that may normalize it in place, and any
// such write would reach straight into the schema the contract validates
// against, racing with concurrent calls. Marshalling through JSON is exact for
// a JSON Schema and leaves nothing aliased.
func cloneSchema(schema *jsonschema.Schema) *jsonschema.Schema {
	if schema == nil {
		return nil
	}
	data, err := jsonv2.Marshal(schema)
	if err != nil {
		return schema.CloneSchemas()
	}
	var clone jsonschema.Schema
	if err := jsonv2.Unmarshal(data, &clone); err != nil {
		return schema.CloneSchemas()
	}
	return &clone
}

func schemaHasType(schema *jsonschema.Schema, want string) bool {
	return schema.Type == want || slices.Contains(schema.Types, want)
}
