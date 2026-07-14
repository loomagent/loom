package loom

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
)

// argumentGuidance is compiled with a ToolContract. expected is deliberately
// not JSON so a model cannot mistake it for a callable argument object.
type argumentGuidance struct {
	expected string
	example  string
}

func buildArgumentGuidance[T any](schema *jsonschema.Schema, resolved *jsonschema.Resolved) (argumentGuidance, error) {
	guidance := argumentGuidance{expected: summarizeExpectedArguments(schema)}
	if err := validateDeclaredExamples(schema, schema, ""); err != nil {
		return argumentGuidance{}, err
	}

	example, complete := buildSchemaExample(schema)
	if !complete {
		return guidance, nil
	}
	if err := resolved.Validate(example); err != nil {
		return argumentGuidance{}, fmt.Errorf("assembled example does not satisfy JSON Schema: %w", err)
	}
	data, err := json.Marshal(example)
	if err != nil {
		return argumentGuidance{}, fmt.Errorf("marshal assembled example: %w", err)
	}
	var typed T
	if err := json.Unmarshal(data, &typed); err != nil {
		return argumentGuidance{}, fmt.Errorf("decode assembled example into argument struct: %w", err)
	}
	if err := validateToolArgumentStruct(typed); err != nil {
		return argumentGuidance{}, fmt.Errorf("assembled example does not satisfy struct validation: %w", err)
	}
	data, err = json.Marshal(typed)
	if err != nil {
		return argumentGuidance{}, fmt.Errorf("marshal validated argument example: %w", err)
	}
	guidance.example = string(data)
	return guidance, nil
}

func validateDeclaredExamples(root, schema *jsonschema.Schema, path string) error {
	if schema == nil {
		return nil
	}
	for index, example := range schema.Examples {
		standalone := schema.CloneSchemas()
		standalone.ID = ""
		standalone.Schema = ""
		standalone.Examples = nil
		standalone.Defs = root.Defs
		standalone.Definitions = root.Definitions
		resolved, err := standalone.Resolve(nil)
		if err != nil {
			return fmt.Errorf("resolve example schema at %s: %w", examplePath(path, index), err)
		}
		if err := resolved.Validate(example); err != nil {
			return fmt.Errorf("example at %s does not satisfy JSON Schema: %w", examplePath(path, index), err)
		}
	}
	for _, name := range orderedPropertyNames(schema) {
		if err := validateDeclaredExamples(root, schema.Properties[name], joinFieldPath(path, name)); err != nil {
			return err
		}
	}
	if err := validateDeclaredExamples(root, schema.Items, path+"[]"); err != nil {
		return err
	}
	if err := validateDeclaredExamples(root, schema.AdditionalProperties, path+"{}"); err != nil {
		return err
	}
	return nil
}

func examplePath(path string, index int) string {
	if path == "" {
		path = "arguments"
	}
	return fmt.Sprintf("%s.examples[%d]", path, index)
}

func buildSchemaExample(schema *jsonschema.Schema) (any, bool) {
	if schema == nil {
		return nil, false
	}
	if len(schema.Examples) > 0 {
		return schema.Examples[0], true
	}
	if !schemaHasType(schema, "object") {
		return nil, false
	}
	object := make(map[string]any)
	for _, name := range orderedPropertyNames(schema) {
		property := schema.Properties[name]
		value, ok := buildSchemaExample(property)
		if ok {
			object[name] = value
			continue
		}
		if slices.Contains(schema.Required, name) {
			return nil, false
		}
	}
	return object, true
}
