package loom

import (
	"fmt"
	"slices"
)

// argumentGuidance is compiled with an ArgsContract. expected is deliberately
// not JSON so a model cannot mistake it for a callable argument object.
type argumentGuidance struct {
	// built distinguishes "not compiled yet" from a contract whose summary is
	// legitimately empty — NoArguments and other property-less schemas produce
	// no expected-argument text, and keying off expected alone would rebuild
	// their guidance on every single decode.
	built    bool
	expected string
	example  string
}

func validateDeclaredExamples(schema *Schema, path string) error {
	if schema == nil {
		return nil
	}
	for index, example := range schema.Examples {
		if err := ValidateSchema(schema, example); err != nil {
			return fmt.Errorf("example at %s does not satisfy JSON Schema: %w", examplePath(path, index), err)
		}
	}
	for _, name := range orderedPropertyNames(schema) {
		if err := validateDeclaredExamples(schema.Properties[name], joinFieldPath(path, name)); err != nil {
			return err
		}
	}
	if err := validateDeclaredExamples(schema.Items, path+"[]"); err != nil {
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

// buildSchemaExample assembles an example instance for schema.
//
// complete reports whether an example could be assembled at all. declared
// reports whether any part of it came from an author-declared example, which
// decides how a later validation failure is treated: a declared example that
// does not satisfy its own schema is an authoring mistake worth failing on,
// while an example the framework assembled on its own is best-effort and may
// simply be dropped.
func buildSchemaExample(schema *Schema) (example any, complete, declared bool) {
	if schema == nil {
		return nil, false, false
	}
	if len(schema.Examples) > 0 {
		return schema.Examples[0], true, true
	}
	if !schemaHasType(schema, "object") {
		return nil, false, false
	}
	object := make(map[string]any)
	for _, name := range orderedPropertyNames(schema) {
		property := schema.Properties[name]
		value, ok, propertyDeclared := buildSchemaExample(property)
		if ok {
			object[name] = value
			declared = declared || propertyDeclared
			continue
		}
		if slices.Contains(schema.Required, name) {
			return nil, false, false
		}
	}
	return object, true, declared
}
