package loom

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/google/jsonschema-go/jsonschema"
)

var toolArgumentValidator = newToolArgumentValidator()

func newToolArgumentValidator() *validator.Validate {
	validate := validator.New(validator.WithRequiredStructEnabled())
	validate.RegisterTagNameFunc(func(field reflect.StructField) string {
		name, _, skip := jsonFieldName(field)
		if skip {
			return ""
		}
		return name
	})
	return validate
}

// SchemaFor derives a JSON Schema from T.
//
// Exported struct fields become object properties. The json tag controls the
// property name and whether it is optional: fields tagged with omitempty or
// omitzero are optional; all other fields are required. The jsonschema tag is
// used as the property description.
//
// A go-playground/validator validate tag adds runtime constraints. The min,
// max, len, oneof, and required rules are also projected into JSON Schema;
// other rules remain runtime-only. Min, max, and len apply to numeric values,
// string lengths, array lengths, or map sizes according to the field type.
func SchemaFor[T any]() (*jsonschema.Schema, error) {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		return nil, err
	}
	if err := applyValidationTags(schema, reflect.TypeFor[T]()); err != nil {
		var value T
		return nil, fmt.Errorf("validation tags for %T: %w", value, err)
	}
	return schema, nil
}

// MustSchemaFor is SchemaFor for statically known request and response types.
// It panics when T cannot be represented as JSON Schema, which indicates a
// programming error in the declared type.
func MustSchemaFor[T any]() *jsonschema.Schema {
	schema, err := SchemaFor[T]()
	if err != nil {
		var value T
		panic(fmt.Sprintf("loom: derive JSON Schema for %T: %v", value, err))
	}
	return schema
}

// DecodeToolArguments validates one JSON tool-call argument object against the
// schema derived from T, then decodes it into T. This keeps model-facing JSON
// Schema and server-side validation on the same contract.
func DecodeToolArguments[T any](argumentsJSON string) (T, error) {
	var zero T
	schema, err := SchemaFor[T]()
	if err != nil {
		return zero, err
	}
	return DecodeToolArgumentsWithSchema[T](argumentsJSON, schema)
}

// DecodeToolArgumentsWithSchema is like DecodeToolArguments, but validates
// against schema. It is useful when a tool adds runtime constraints, such as a
// configurable maximum, to a schema initially derived from T. The schema must
// still describe T.
func DecodeToolArgumentsWithSchema[T any](argumentsJSON string, schema *jsonschema.Schema) (T, error) {
	var zero T
	if schema == nil {
		return zero, fmt.Errorf("loom: tool argument schema is nil")
	}

	decoder := json.NewDecoder(strings.NewReader(argumentsJSON))
	var instance any
	if err := decoder.Decode(&instance); err != nil {
		return zero, fmt.Errorf("loom: parse tool arguments: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return zero, fmt.Errorf("loom: parse tool arguments: %w", err)
	}

	resolved, err := schema.Resolve(nil)
	if err != nil {
		return zero, fmt.Errorf("loom: resolve tool argument schema: %w", err)
	}
	if err := resolved.Validate(instance); err != nil {
		return zero, fmt.Errorf("loom: validate tool arguments: %w", err)
	}

	var arguments T
	decoder = json.NewDecoder(strings.NewReader(argumentsJSON))
	if err := decoder.Decode(&arguments); err != nil {
		return zero, fmt.Errorf("loom: decode tool arguments: %w", err)
	}
	if err := validateToolArgumentStruct(arguments); err != nil {
		return zero, fmt.Errorf("loom: validate tool argument struct: %w", err)
	}
	return arguments, nil
}

func validateToolArgumentStruct(value any) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("invalid validator tag: %v", recovered)
		}
	}()
	return toolArgumentValidator.Struct(value)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func applyValidationTags(schema *jsonschema.Schema, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		if !schemaHasType(schema, "object") {
			return nil
		}
		for i := range typ.NumField() {
			field := typ.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name, explicit, skip := jsonFieldName(field)
			if skip {
				continue
			}
			if field.Anonymous && !explicit {
				if err := applyValidationTags(schema, field.Type); err != nil {
					return err
				}
				continue
			}
			property := schema.Properties[name]
			if property == nil {
				continue
			}
			if tag, ok := field.Tag.Lookup("validate"); ok {
				if err := applyValidationRules(property, field.Type, tag); err != nil {
					return fmt.Errorf("field %s: %w", field.Name, err)
				}
				if hasValidationRule(tag, "required") && !slices.Contains(schema.Required, name) {
					schema.Required = append(schema.Required, name)
				}
			}
			if err := applyValidationTags(property, field.Type); err != nil {
				return err
			}
		}
	case reflect.Array, reflect.Slice:
		if schema.Items != nil {
			return applyValidationTags(schema.Items, typ.Elem())
		}
	case reflect.Map:
		if schema.AdditionalProperties != nil {
			return applyValidationTags(schema.AdditionalProperties, typ.Elem())
		}
	}
	return nil
}

func schemaHasType(schema *jsonschema.Schema, want string) bool {
	return schema.Type == want || slices.Contains(schema.Types, want)
}

func jsonFieldName(field reflect.StructField) (name string, explicit, skip bool) {
	tag, ok := field.Tag.Lookup("json")
	if ok {
		name, _, _ = strings.Cut(tag, ",")
		if name == "-" {
			return "", true, true
		}
		if name != "" {
			return name, true, false
		}
	}
	return field.Name, false, false
}

func applyValidationRules(schema *jsonschema.Schema, typ reflect.Type, tag string) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	for rule := range strings.SplitSeq(tag, ",") {
		rule = strings.TrimSpace(rule)
		if rule == "" || rule == "required" || rule == "omitempty" || rule == "omitzero" {
			continue
		}
		name, value, ok := strings.Cut(rule, "=")
		switch name {
		case "min", "max", "len":
			if !ok || value == "" {
				return fmt.Errorf("invalid validate rule %q", rule)
			}
			if err := applySizeRule(schema, typ.Kind(), name, value); err != nil {
				return err
			}
		case "oneof":
			if !ok || value == "" {
				return fmt.Errorf("invalid validate rule %q", rule)
			}
			values := strings.Fields(value)
			if len(values) == 0 {
				return fmt.Errorf("oneof requires at least one value")
			}
			schema.Enum = make([]any, len(values))
			for i, raw := range values {
				parsed, err := parseEnumValue(typ.Kind(), raw)
				if err != nil {
					return fmt.Errorf("oneof value %q: %w", raw, err)
				}
				schema.Enum[i] = parsed
			}
		default:
			// go-playground/validator remains the source of truth for runtime
			// validation. Rules without a direct JSON Schema equivalent are
			// intentionally left to it.
			continue
		}
	}
	return nil
}

func hasValidationRule(tag, want string) bool {
	for rule := range strings.SplitSeq(tag, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(rule), "=")
		if name == want {
			return true
		}
	}
	return false
}

func applySizeRule(schema *jsonschema.Schema, kind reflect.Kind, rule, raw string) error {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%s=%q is not numeric", rule, raw)
		}
		switch rule {
		case "min":
			schema.Minimum = &value
		case "max":
			schema.Maximum = &value
		default:
			return fmt.Errorf("len is not valid for numeric fields")
		}
	case reflect.String, reflect.Array, reflect.Slice, reflect.Map:
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return fmt.Errorf("%s=%q must be a non-negative integer", rule, raw)
		}
		setLengthRule(schema, kind, rule, value)
	default:
		return fmt.Errorf("%s is not valid for %s fields", rule, kind)
	}
	return nil
}

func setLengthRule(schema *jsonschema.Schema, kind reflect.Kind, rule string, value int) {
	set := func(minimum, maximum **int) {
		switch rule {
		case "min":
			*minimum = &value
		case "max":
			*maximum = &value
		case "len":
			*minimum, *maximum = &value, &value
		}
	}
	switch kind {
	case reflect.String:
		set(&schema.MinLength, &schema.MaxLength)
	case reflect.Array, reflect.Slice:
		set(&schema.MinItems, &schema.MaxItems)
	case reflect.Map:
		set(&schema.MinProperties, &schema.MaxProperties)
	}
}

func parseEnumValue(kind reflect.Kind, raw string) (any, error) {
	switch kind {
	case reflect.String:
		return raw, nil
	case reflect.Bool:
		return strconv.ParseBool(raw)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.ParseInt(raw, 10, 64)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.ParseUint(raw, 10, 64)
	case reflect.Float32, reflect.Float64:
		return strconv.ParseFloat(raw, 64)
	default:
		return nil, fmt.Errorf("oneof is not valid for %s fields", kind)
	}
}
