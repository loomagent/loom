package loom

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/loomagent/loom/internal/toolcontract"
)

var errMultipleJSONValues = errors.New("multiple JSON values")

var toolArgumentValidator = newToolArgumentValidator()

func newToolArgumentValidator() *validator.Validate {
	validate := validator.New(validator.WithRequiredStructEnabled())
	if err := validate.RegisterValidation("notblank", func(field validator.FieldLevel) bool {
		return field.Field().Kind() == reflect.String && strings.TrimSpace(field.Field().String()) != ""
	}); err != nil {
		panic(fmt.Sprintf("loom: register notblank validation: %v", err))
	}
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
// A go-playground/validator validate tag adds runtime constraints. Rules with
// direct JSON Schema equivalents are projected as well; cross-field, custom,
// and other runtime-only rules remain enforced by validator. Container rules
// after dive are projected onto item or map-value schemas.
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
	return DecodeToolArgumentsFor[T]("", argumentsJSON)
}

// DecodeToolArgumentsFor is DecodeToolArguments with a tool name included in
// validation errors.
func DecodeToolArgumentsFor[T any](toolName, argumentsJSON string) (T, error) {
	var zero T
	schema, err := SchemaFor[T]()
	if err != nil {
		return zero, err
	}
	return DecodeToolArgumentsWithSchemaFor[T](toolName, argumentsJSON, schema)
}

// DecodeToolArgumentsWithSchema is like DecodeToolArguments, but validates
// against schema. It is useful when a tool adds runtime constraints, such as a
// configurable maximum, to a schema initially derived from T. The schema must
// still describe T.
func DecodeToolArgumentsWithSchema[T any](argumentsJSON string, schema *jsonschema.Schema) (T, error) {
	return DecodeToolArgumentsWithSchemaFor[T]("", argumentsJSON, schema)
}

// DecodeToolArgumentsWithSchemaFor is DecodeToolArgumentsWithSchema with a
// tool name included in validation errors.
//
// Like the other DecodeToolArguments functions, this resolves the schema and
// rebuilds the argument guidance on every call, which costs roughly an order of
// magnitude more than a precompiled contract. Use it for one-off decoding;
// anything on a request path should build a ToolContract once and call its
// Decode method instead.
func DecodeToolArgumentsWithSchemaFor[T any](toolName, argumentsJSON string, schema *jsonschema.Schema) (T, error) {
	return decodeToolArguments[T](toolName, argumentsJSON, schema, nil, nil, argumentGuidance{})
}

func decodeToolArguments[T any](toolName, argumentsJSON string, schema *jsonschema.Schema, resolved *jsonschema.Resolved, validator *toolcontract.Validator, guidance argumentGuidance) (T, error) {
	var zero T
	if schema == nil {
		return zero, fmt.Errorf("loom: tool argument schema is nil")
	}

	if resolved == nil {
		var err error
		resolved, err = schema.Resolve(nil)
		if err != nil {
			return zero, fmt.Errorf("loom: resolve tool argument schema: %w", err)
		}
	}
	if !guidance.built {
		var err error
		guidance, err = buildArgumentGuidance[T](schema, resolved)
		if err != nil {
			return zero, fmt.Errorf("loom: build tool argument guidance: %w", err)
		}
	}
	if validator == nil {
		var err error
		validator, err = compileValidationSchema(schema)
		if err != nil {
			return zero, fmt.Errorf("loom: compile tool argument schema: %w", err)
		}
	}

	// Providers commonly send an empty string rather than "{}" when a model
	// calls a tool that takes no arguments, or none of its optional ones. Treat
	// blank arguments as an empty JSON object so this convention is not
	// reported as malformed JSON, and so a call that is genuinely missing a
	// required field gets a field-level diagnostic instead of a syntax error.
	if strings.TrimSpace(argumentsJSON) == "" {
		argumentsJSON = "{}"
	}

	raw, err := readStrictJSON(argumentsJSON)
	if err != nil {
		return zero, newJSONToolArgumentError(toolName, guidance, err)
	}

	if validationError := validator.Validate(raw); validationError != nil {
		return zero, newSchemaToolArgumentError(toolName, guidance, validationError)
	}

	var arguments T
	if err := jsonv2.Unmarshal(raw, &arguments, jsonv2.RejectUnknownMembers(true)); err != nil {
		if typeError, ok := errors.AsType[*jsonv2.SemanticError](err); ok {
			return zero, newTypeMismatchToolArgumentError(toolName, guidance, typeError)
		}
		return zero, newJSONToolArgumentError(toolName, guidance, err)
	}
	if err := validateToolArgumentStruct(arguments); err != nil {
		return zero, newStructToolArgumentError(toolName, guidance, err)
	}
	return arguments, nil
}

func compileValidationSchema(schema *jsonschema.Schema) (*toolcontract.Validator, error) {
	data, err := jsonv2.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return toolcontract.Compile(data)
}

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

func validateToolArgumentStruct(value any) (err error) {
	// go-playground only validates structs. Arguments typed as a slice, map, or
	// scalar carry no struct rules, and handing one to Struct yields an
	// InvalidValidationError that reaches the model as "validation is
	// misconfigured" — which blames the tool for a perfectly good call.
	target := reflect.ValueOf(value)
	for target.Kind() == reflect.Pointer {
		if target.IsNil() {
			return nil
		}
		target = target.Elem()
	}
	if target.Kind() != reflect.Struct {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("invalid validator tag: %v", recovered)
		}
	}()
	return toolArgumentValidator.Struct(value)
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
				if err := applyFieldValidationRules(property, field.Type, tag); err != nil {
					return fmt.Errorf("field %s: %w", field.Name, err)
				}
				if hasValidationRule(tag, "required") && !slices.Contains(schema.Required, name) {
					schema.Required = append(schema.Required, name)
				}
			}
			// Declared examples are attached after the validate tag is projected:
			// that projection may copy the schema through JSON, which would
			// renormalize a parsed example's Go type (int64 becoming float64).
			if raw, ok := field.Tag.Lookup("example"); ok {
				example, err := parseExampleValue(field.Type, raw)
				if err != nil {
					return fmt.Errorf("field %s example: %w", field.Name, err)
				}
				property.Examples = []any{example}
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
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.Interface, reflect.Pointer, reflect.Invalid,
		reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// Only composite kinds carry nested fields to walk into; a scalar's own
		// validate tag was already projected by its parent struct.
	default:
		// Unreachable: every reflect.Kind is listed above.
	}
	return nil
}

// applyFieldValidationRules projects one field's validate tag onto its schema.
//
// A leading omitempty (or omitzero/omitnil) makes go-playground skip every
// later rule when the value is empty, and JSON Schema has no way to express
// that exemption. Projecting such rules verbatim makes the schema stricter
// than the validator that actually decides: an explicit "" would be rejected
// by the schema while the validator accepts it. So the projection is trialled
// on a copy first and kept only when the field's own empty value still
// satisfies it. Constraints that empty values satisfy anyway — uniqueItems, an
// upper bound — survive; the ones that would contradict the validator, such as
// minLength or enum, are dropped and left to the validator alone.
func applyFieldValidationRules(property *jsonschema.Schema, typ reflect.Type, tag string) error {
	exempt := hasEmptyExemption(tag)
	target := property
	if exempt {
		target = cloneSchema(property)
	}
	if err := applyValidationRules(target, typ, tag); err != nil {
		return err
	}
	if exempt {
		if rejectsEmptyValue(target, typ) {
			return nil
		}
		*property = *target
	}
	return nil
}

func hasEmptyExemption(tag string) bool {
	for _, rule := range splitValidationRules(tag) {
		switch rule {
		case "dive":
			// Later rules apply to elements, not to this value.
			return false
		case "omitempty", "omitzero", "omitnil":
			return true
		}
	}
	return false
}

// rejectsEmptyValue reports whether schema turns away the value go-playground
// treats as empty for typ. Types with no meaningful JSON empty value, and
// schemas that cannot be resolved standalone, report false so the projection is
// kept as-is.
func rejectsEmptyValue(schema *jsonschema.Schema, typ reflect.Type) bool {
	empty, ok := jsonEmptyValue(typ)
	if !ok {
		return false
	}
	resolved, err := cloneSchema(schema).Resolve(nil)
	if err != nil {
		return false
	}
	return resolved.Validate(empty) != nil
}

func jsonEmptyValue(typ reflect.Type) (any, bool) {
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == nil {
		return nil, false
	}
	switch typ.Kind() {
	case reflect.String:
		return "", true
	case reflect.Bool:
		return false, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return int64(0), true
	case reflect.Float32, reflect.Float64:
		return float64(0), true
	case reflect.Slice, reflect.Array:
		return []any{}, true
	case reflect.Map:
		return map[string]any{}, true
	case reflect.Struct, reflect.Interface, reflect.Invalid, reflect.Pointer,
		reflect.Chan, reflect.Func, reflect.UnsafePointer,
		reflect.Complex64, reflect.Complex128:
		// A struct has no single empty JSON form worth testing, and the rest
		// never appear in a JSON-serializable argument type.
		return nil, false
	default:
		// Unreachable: every reflect.Kind is listed above.
		return nil, false
	}
}

// parseJSONExampleValue reads an example written as a JSON literal, used for
// kinds with no scalar textual form.
func parseJSONExampleValue(typ reflect.Type, raw string) (any, error) {
	var value any
	if err := jsonv2.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("must be valid JSON for %s: %w", typ, err)
	}
	return value, nil
}

func parseExampleValue(typ reflect.Type, raw string) (any, error) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.String:
		return raw, nil
	case reflect.Bool:
		return strconv.ParseBool(raw)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.ParseInt(raw, 10, typ.Bits())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.ParseUint(raw, 10, typ.Bits())
	case reflect.Float32, reflect.Float64:
		return strconv.ParseFloat(raw, typ.Bits())
	case reflect.Slice, reflect.Array, reflect.Map, reflect.Struct,
		reflect.Interface, reflect.Pointer, reflect.Invalid,
		reflect.Chan, reflect.Func, reflect.UnsafePointer,
		reflect.Complex64, reflect.Complex128:
		// Composite and unsupported kinds are read as raw JSON: an example for
		// them is written as a JSON literal in the struct tag.
		return parseJSONExampleValue(typ, raw)
	default:
		// Unreachable: every reflect.Kind is listed above.
		return parseJSONExampleValue(typ, raw)
	}
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
	return applyValidationRuleList(schema, typ, splitValidationRules(tag))
}

func splitValidationRules(tag string) []string {
	rules := strings.Split(tag, ",")
	for index := range rules {
		rules[index] = strings.TrimSpace(rules[index])
	}
	return rules
}

func applyValidationRuleList(schema *jsonschema.Schema, typ reflect.Type, rules []string) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	// After omitempty (or omitzero/omitnil) go-playground skips the remaining
	// rules whenever the value is empty. JSON Schema cannot express that
	// exemption, so from here on each rule is trialled on a copy and kept only
	// if the field's empty value still satisfies it. Constraints an empty value
	// meets anyway — uniqueItems, an upper bound — survive; ones that would
	// contradict the validator, such as minItems or minLength, are dropped.
	exemptEmpty := false
	for index, rule := range rules {
		if rule == "dive" {
			// Rules after dive constrain elements, not this value, so the
			// exemption does not carry into them.
			return applyDiveValidationRules(schema, typ, rules[index+1:])
		}
		if rule == "keys" || rule == "endkeys" {
			return fmt.Errorf("%q must follow dive on a map", rule)
		}
		if rule == "omitempty" || rule == "omitzero" || rule == "omitnil" {
			exemptEmpty = true
			continue
		}
		if rule == "" || rule == "structonly" || rule == "nostructlevel" {
			continue
		}
		if exemptEmpty {
			trial := cloneSchema(schema)
			if err := applySingleValidationRule(trial, typ, rule); err != nil {
				return err
			}
			if !rejectsEmptyValue(trial, typ) {
				*schema = *trial
			}
			continue
		}
		if err := applySingleValidationRule(schema, typ, rule); err != nil {
			return err
		}
	}
	return nil
}

func applySingleValidationRule(schema *jsonschema.Schema, typ reflect.Type, rule string) error {
	{
		if strings.Contains(rule, "|") {
			return applyOrValidationRule(schema, typ, rule)
		}
		name, value, ok := strings.Cut(rule, "=")
		value = decodeValidatorParam(value)
		switch name {
		case "required":
			applyRequiredValueRule(schema, typ.Kind())
		case "min", "max", "len":
			if !ok || value == "" {
				return fmt.Errorf("invalid validate rule %q", rule)
			}
			if err := applySizeRule(schema, typ.Kind(), name, value); err != nil {
				return err
			}
		case "gt", "gte", "lt", "lte":
			if !ok || value == "" {
				return fmt.Errorf("invalid validate rule %q", rule)
			}
			if err := applyComparisonRule(schema, typ.Kind(), name, value); err != nil {
				return err
			}
		case "eq", "ne":
			if !ok || value == "" {
				return fmt.Errorf("invalid validate rule %q", rule)
			}
			if err := applyEqualityRule(schema, typ.Kind(), name, value); err != nil {
				return err
			}
		case "oneof":
			if !ok || value == "" {
				return fmt.Errorf("invalid validate rule %q", rule)
			}
			if !validatorOneOfKind(typ.Kind()) {
				return fmt.Errorf("oneof is not valid for %s fields", typ.Kind())
			}
			values := parseOneOfValues(value)
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
		case "contains", "startswith", "endswith", "excludes":
			if !ok || value == "" || typ.Kind() != reflect.String {
				return fmt.Errorf("%s requires a non-empty value on a string field", name)
			}
			pattern := regexp.QuoteMeta(value)
			switch name {
			case "startswith":
				pattern = "^" + pattern
			case "endswith":
				pattern += "$"
			case "excludes":
				addNotConstraint(schema, &jsonschema.Schema{Pattern: pattern})
				return nil
			}
			addPatternConstraint(schema, pattern)
		case "unique":
			if ok && value != "" {
				return nil // unique=Field has no direct JSON Schema equivalent.
			}
			if typ.Kind() == reflect.Array || typ.Kind() == reflect.Slice {
				schema.UniqueItems = true
			}
		case "email", "url", "uri", "hostname", "ipv4", "ipv6", "uuid", "uuid3", "uuid4", "uuid5", "datetime":
			if typ.Kind() != reflect.String {
				return fmt.Errorf("%s is only valid for string fields", name)
			}
			if format := validatorJSONSchemaFormat(name, value); format != "" {
				schema.Format = format
			}
		case "notblank":
			if typ.Kind() != reflect.String {
				return fmt.Errorf("notblank is only valid for string fields")
			}
			addPatternConstraint(schema, `\S`)
		default:
			// go-playground/validator remains the source of truth for runtime
			// validation. Rules without a direct JSON Schema equivalent are
			// intentionally left to it.
			return nil
		}
	}
	return nil
}

func applyOrValidationRule(schema *jsonschema.Schema, typ reflect.Type, rule string) error {
	alternatives := strings.Split(rule, "|")
	branches := make([]*jsonschema.Schema, 0, len(alternatives))
	for _, alternative := range alternatives {
		branch := &jsonschema.Schema{}
		if err := applyValidationRuleList(branch, typ, []string{alternative}); err != nil {
			return fmt.Errorf("validation alternative %q: %w", alternative, err)
		}
		if isVacuousSchema(branch) {
			// This alternative asserts nothing, so it matches anything — and an
			// anyOf containing it constrains nothing while still looking like a
			// constraint. Drop the whole projection and let the validator, which
			// does enforce the rule, be the only word on it.
			return nil
		}
		branches = append(branches, branch)
	}
	schema.AllOf = append(schema.AllOf, &jsonschema.Schema{AnyOf: branches})
	return nil
}

// annotationSchemaKeywords carry no assertion: a schema holding only these
// accepts every instance. format is among them because this library's validator
// treats it as an annotation rather than a constraint.
var annotationSchemaKeywords = map[string]bool{
	"format": true, "description": true, "title": true, "examples": true,
	"default": true, "deprecated": true, "readOnly": true, "writeOnly": true,
	"$comment": true, "$schema": true, "$id": true,
}

// isVacuousSchema reports whether schema accepts every instance — either an
// empty schema, or one carrying nothing but annotations.
func isVacuousSchema(schema *jsonschema.Schema) bool {
	if schema == nil {
		return true
	}
	data, err := jsonv2.Marshal(schema)
	if err != nil {
		return false
	}
	if string(data) == "true" {
		return true
	}
	var keywords map[string]any
	if err := jsonv2.Unmarshal(data, &keywords); err != nil {
		return false
	}
	for keyword := range keywords {
		if !annotationSchemaKeywords[keyword] {
			return false
		}
	}
	return true
}

// validatorJSONSchemaFormat maps a validator rule to a JSON Schema format, or
// returns "" when no format describes it faithfully. param carries the rule's
// argument, which matters for datetime: its layout decides whether the value is
// a date, a time, or a full timestamp, and claiming date-time for a date-only
// layout tells the model to send a value the validator will reject.
func validatorJSONSchemaFormat(rule, param string) string {
	switch rule {
	case "url", "uri":
		return "uri"
	case "uuid3", "uuid4", "uuid5":
		return "uuid"
	case "datetime":
		switch param {
		case "2006-01-02":
			return "date"
		case "15:04:05":
			return "time"
		case "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05":
			return "date-time"
		default:
			// A custom layout has no JSON Schema format; the validator enforces
			// it, and the error message names the layout.
			return ""
		}
	default:
		return rule
	}
}

func validatorOneOfKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	case reflect.Bool, reflect.Uintptr, reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.Array, reflect.Slice, reflect.Map, reflect.Struct,
		reflect.Interface, reflect.Pointer, reflect.Invalid,
		reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// oneof enumerates scalar values; a float cannot be compared exactly
		// and the composite kinds have no enumerable form.
		return false
	default:
		// Unreachable: every reflect.Kind is listed above.
		return false
	}
}

func applyDiveValidationRules(schema *jsonschema.Schema, typ reflect.Type, rules []string) error {
	switch typ.Kind() {
	case reflect.Array, reflect.Slice:
		if schema.Items == nil {
			return fmt.Errorf("dive requires an item schema")
		}
		return applyValidationRuleList(schema.Items, typ.Elem(), rules)
	case reflect.Map:
		if len(rules) > 0 && rules[0] == "keys" {
			end := slices.Index(rules, "endkeys")
			if end < 0 {
				return fmt.Errorf("keys requires a matching endkeys")
			}
			if schema.PropertyNames == nil {
				schema.PropertyNames = &jsonschema.Schema{Type: "string"}
			}
			if err := applyValidationRuleList(schema.PropertyNames, typ.Key(), rules[1:end]); err != nil {
				return fmt.Errorf("map keys: %w", err)
			}
			rules = rules[end+1:]
		}
		if schema.AdditionalProperties == nil {
			return fmt.Errorf("dive requires a map value schema")
		}
		return applyValidationRuleList(schema.AdditionalProperties, typ.Elem(), rules)
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.Struct, reflect.Interface, reflect.Pointer, reflect.Invalid,
		reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// dive descends into elements, which only arrays, slices and maps have.
		return fmt.Errorf("dive is not valid for %s fields", typ.Kind())
	default:
		// Unreachable: every reflect.Kind is listed above.
		return fmt.Errorf("dive is not valid for %s fields", typ.Kind())
	}
}

func decodeValidatorParam(value string) string {
	value = strings.ReplaceAll(value, "0x2C", ",")
	return strings.ReplaceAll(value, "0x7C", "|")
}

var oneOfValuePattern = regexp.MustCompile(`'[^']*'|\S+`)

func parseOneOfValues(value string) []string {
	values := oneOfValuePattern.FindAllString(value, -1)
	for index := range values {
		values[index] = strings.ReplaceAll(values[index], "'", "")
	}
	return values
}

func applyRequiredValueRule(schema *jsonschema.Schema, kind reflect.Kind) {
	one := 1
	switch kind {
	case reflect.String:
		setStrongestMinimum(&schema.MinLength, one)
	case reflect.Array, reflect.Slice:
		setStrongestMinimum(&schema.MinItems, one)
	case reflect.Map:
		setStrongestMinimum(&schema.MinProperties, one)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		zero := any(float64(0))
		addNotConstraint(schema, &jsonschema.Schema{Const: &zero})
	case reflect.Bool:
		value := any(true)
		schema.Const = &value
	case reflect.Struct, reflect.Interface, reflect.Pointer, reflect.Invalid,
		reflect.Chan, reflect.Func, reflect.UnsafePointer,
		reflect.Complex64, reflect.Complex128:
		// No "non-zero" form to project: a struct or interface has no single
		// empty value, and the rest never reach a JSON argument schema.
	default:
		// Unreachable: every reflect.Kind is listed above.
	}
}

// parseNumericRuleParam parses a numeric validator bound for projection into a
// JSON Schema minimum/maximum. Integer literals are rejected when float64
// cannot hold them exactly: projecting a rounded bound would silently reject
// values the validator accepts, so it is better to project nothing and let the
// validator remain the only enforcer.
func parseNumericRuleParam(raw string) (float64, error) {
	if integer, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if int64(float64(integer)) != integer {
			return 0, fmt.Errorf("%q is not exactly representable as a JSON Schema bound", raw)
		}
		return float64(integer), nil
	}
	if unsigned, err := strconv.ParseUint(raw, 10, 64); err == nil {
		if uint64(float64(unsigned)) != unsigned {
			return 0, fmt.Errorf("%q is not exactly representable as a JSON Schema bound", raw)
		}
		return float64(unsigned), nil
	}
	return strconv.ParseFloat(raw, 64)
}

func applyComparisonRule(schema *jsonschema.Schema, kind reflect.Kind, rule, raw string) error {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		value, err := parseNumericRuleParam(raw)
		if err != nil {
			// Not a plain number — go-playground also accepts forms such as a
			// time.Duration bound ("min=1s"). JSON Schema has no equivalent, so
			// skip the projection and let the validator enforce it.
			return nil
		}
		switch rule {
		case "gt":
			schema.ExclusiveMinimum = strongestLowerBound(schema.ExclusiveMinimum, value)
		case "gte":
			schema.Minimum = strongestLowerBound(schema.Minimum, value)
		case "lt":
			schema.ExclusiveMaximum = strongestUpperBound(schema.ExclusiveMaximum, value)
		case "lte":
			schema.Maximum = strongestUpperBound(schema.Maximum, value)
		}
	case reflect.String, reflect.Array, reflect.Slice, reflect.Map:
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			// Length bounds only accept non-negative integers; anything else has
			// no JSON Schema projection. Leave it to the validator.
			return nil
		}
		switch rule {
		case "gt":
			value++
			setLengthRule(schema, kind, "min", value)
		case "gte":
			setLengthRule(schema, kind, "min", value)
		case "lt":
			if value == 0 {
				return fmt.Errorf("lt=0 cannot be satisfied by a %s", kind)
			}
			value--
			setLengthRule(schema, kind, "max", value)
		case "lte":
			setLengthRule(schema, kind, "max", value)
		}
	case reflect.Bool, reflect.Complex64, reflect.Complex128,
		reflect.Struct, reflect.Interface, reflect.Pointer, reflect.Invalid,
		reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// A bound needs either a length or a numeric value; these kinds have
		// neither, so the rule cannot apply to them.
		return fmt.Errorf("%s is not valid for %s fields", rule, kind)
	default:
		// Unreachable: every reflect.Kind is listed above.
		return fmt.Errorf("%s is not valid for %s fields", rule, kind)
	}
	return nil
}

func applyEqualityRule(schema *jsonschema.Schema, kind reflect.Kind, rule, raw string) error {
	if kind == reflect.Array || kind == reflect.Slice || kind == reflect.Map {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return fmt.Errorf("%s=%q must be a non-negative integer", rule, raw)
		}
		if rule == "eq" {
			setLengthRule(schema, kind, "len", value)
		}
		return nil // ne=<length> cannot be represented by one JSON Schema bound.
	}
	value, err := parseEnumValue(kind, raw)
	if err != nil {
		return fmt.Errorf("%s value %q: %w", rule, raw, err)
	}
	if rule == "eq" {
		schema.Const = &value
	} else {
		addNotConstraint(schema, &jsonschema.Schema{Const: &value})
	}
	return nil
}

func addPatternConstraint(schema *jsonschema.Schema, pattern string) {
	if schema.Pattern == "" {
		schema.Pattern = pattern
		return
	}
	schema.AllOf = append(schema.AllOf, &jsonschema.Schema{Pattern: pattern})
}

func addNotConstraint(schema *jsonschema.Schema, constraint *jsonschema.Schema) {
	if schema.Not == nil {
		schema.Not = constraint
		return
	}
	schema.AllOf = append(schema.AllOf, &jsonschema.Schema{Not: constraint})
}

func strongestLowerBound(current *float64, value float64) *float64 {
	if current == nil || value > *current {
		return &value
	}
	return current
}

func strongestUpperBound(current *float64, value float64) *float64 {
	if current == nil || value < *current {
		return &value
	}
	return current
}

func setStrongestMinimum(current **int, value int) {
	if *current == nil || value > **current {
		*current = &value
	}
}

func setStrongestMaximum(current **int, value int) {
	if *current == nil || value < **current {
		*current = &value
	}
}

func hasValidationRule(tag, want string) bool {
	for rule := range strings.SplitSeq(tag, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(rule), "=")
		if name == "dive" {
			return false
		}
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
		value, err := parseNumericRuleParam(raw)
		if err != nil {
			// Not a plain number — go-playground also accepts forms such as a
			// time.Duration bound ("min=1s"). JSON Schema has no equivalent, so
			// skip the projection and let the validator enforce it.
			return nil
		}
		switch rule {
		case "min":
			schema.Minimum = strongestLowerBound(schema.Minimum, value)
		case "max":
			schema.Maximum = strongestUpperBound(schema.Maximum, value)
		default:
			return fmt.Errorf("len is not valid for numeric fields")
		}
	case reflect.String, reflect.Array, reflect.Slice, reflect.Map:
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			// Length bounds only accept non-negative integers; anything else has
			// no JSON Schema projection. Leave it to the validator.
			return nil
		}
		setLengthRule(schema, kind, rule, value)
	case reflect.Bool, reflect.Complex64, reflect.Complex128,
		reflect.Struct, reflect.Interface, reflect.Pointer, reflect.Invalid,
		reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// A bound needs either a length or a numeric value; these kinds have
		// neither, so the rule cannot apply to them.
		return fmt.Errorf("%s is not valid for %s fields", rule, kind)
	default:
		// Unreachable: every reflect.Kind is listed above.
		return fmt.Errorf("%s is not valid for %s fields", rule, kind)
	}
	return nil
}

func setLengthRule(schema *jsonschema.Schema, kind reflect.Kind, rule string, value int) {
	set := func(minimum, maximum **int) {
		switch rule {
		case "min":
			setStrongestMinimum(minimum, value)
		case "max":
			setStrongestMaximum(maximum, value)
		case "len":
			// Separate variables: pointing both bounds at one int makes a later
			// in-place tweak of the maximum silently move the minimum too, and
			// CloneSchemas preserves that aliasing.
			lower, upper := value, value
			*minimum, *maximum = &lower, &upper
		}
	}
	switch kind {
	case reflect.String:
		set(&schema.MinLength, &schema.MaxLength)
	case reflect.Array, reflect.Slice:
		set(&schema.MinItems, &schema.MaxItems)
	case reflect.Map:
		set(&schema.MinProperties, &schema.MaxProperties)
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.Invalid, reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Pointer, reflect.Struct, reflect.UnsafePointer:
		// Length bounds only apply to strings and containers; numeric bounds
		// are handled by the caller as minimum/maximum instead.
	default:
		// Unreachable: every reflect.Kind is listed above.
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
	case reflect.Complex64, reflect.Complex128,
		reflect.Array, reflect.Slice, reflect.Map, reflect.Struct,
		reflect.Interface, reflect.Pointer, reflect.Invalid,
		reflect.Chan, reflect.Func, reflect.UnsafePointer:
		// oneof enumerates scalar values; these kinds have no literal form to
		// enumerate.
		return nil, fmt.Errorf("oneof is not valid for %s fields", kind)
	default:
		// Unreachable: every reflect.Kind is listed above.
		return nil, fmt.Errorf("oneof is not valid for %s fields", kind)
	}
}
