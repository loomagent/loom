package loom

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/google/jsonschema-go/jsonschema"
)

var errMultipleJSONValues = errors.New("multiple JSON values")

type unsupportedJSONNumberError struct {
	number string
	reason string
}

func (e *unsupportedJSONNumberError) Error() string {
	return fmt.Sprintf("JSON number %q %s", e.number, e.reason)
}

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
func DecodeToolArgumentsWithSchemaFor[T any](toolName, argumentsJSON string, schema *jsonschema.Schema) (T, error) {
	return decodeToolArguments[T](toolName, argumentsJSON, schema, nil, argumentGuidance{})
}

func decodeToolArguments[T any](toolName, argumentsJSON string, schema *jsonschema.Schema, resolved *jsonschema.Resolved, guidance argumentGuidance) (T, error) {
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
	if guidance.expected == "" {
		var err error
		guidance, err = buildArgumentGuidance[T](schema, resolved)
		if err != nil {
			return zero, fmt.Errorf("loom: build tool argument guidance: %w", err)
		}
	}

	decoder := json.NewDecoder(strings.NewReader(argumentsJSON))
	decoder.UseNumber()
	var instance any
	if err := decoder.Decode(&instance); err != nil {
		return zero, newJSONToolArgumentError(toolName, guidance, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return zero, newJSONToolArgumentError(toolName, guidance, err)
	}
	instance, err := normalizeJSONNumbers(instance)
	if err != nil {
		return zero, newJSONToolArgumentError(toolName, guidance, err)
	}
	if err := resolved.Validate(instance); err != nil {
		return zero, newSchemaToolArgumentError(toolName, schema, guidance, instance, err)
	}

	var arguments T
	normalizedJSON, err := json.Marshal(instance)
	if err != nil {
		return zero, newJSONToolArgumentError(toolName, guidance, err)
	}
	decoder = json.NewDecoder(strings.NewReader(string(normalizedJSON)))
	if err := decoder.Decode(&arguments); err != nil {
		return zero, newJSONToolArgumentError(toolName, guidance, err)
	}
	if err := validateToolArgumentStruct(arguments); err != nil {
		return zero, newStructToolArgumentError(toolName, guidance, err)
	}
	return arguments, nil
}

func normalizeJSONNumbers(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		return normalizeJSONNumber(value)
	case []any:
		for index := range value {
			normalized, err := normalizeJSONNumbers(value[index])
			if err != nil {
				return nil, fmt.Errorf("array item %d: %w", index, err)
			}
			value[index] = normalized
		}
		return value, nil
	case map[string]any:
		for name, item := range value {
			normalized, err := normalizeJSONNumbers(item)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", name, err)
			}
			value[name] = normalized
		}
		return value, nil
	default:
		return value, nil
	}
}

func normalizeJSONNumber(number json.Number) (any, error) {
	rational, ok := new(big.Rat).SetString(number.String())
	if !ok {
		return nil, &unsupportedJSONNumberError{number: number.String(), reason: "cannot be represented"}
	}
	if rational.IsInt() {
		integer := rational.Num()
		if integer.IsInt64() {
			return integer.Int64(), nil
		}
		if integer.Sign() >= 0 && integer.BitLen() <= 64 {
			return integer.Uint64(), nil
		}
		return nil, &unsupportedJSONNumberError{number: number.String(), reason: "is outside the supported 64-bit integer range"}
	}
	floating, _ := rational.Float64()
	if math.IsInf(floating, 0) {
		return nil, &unsupportedJSONNumberError{number: number.String(), reason: "is outside the supported floating-point range"}
	}
	return floating, nil
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
			return errMultipleJSONValues
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
			if raw, ok := field.Tag.Lookup("example"); ok {
				example, err := parseExampleValue(field.Type, raw)
				if err != nil {
					return fmt.Errorf("field %s example: %w", field.Name, err)
				}
				property.Examples = []any{example}
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
	default:
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return nil, fmt.Errorf("must be valid JSON for %s: %w", typ, err)
		}
		return value, nil
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
	for index, rule := range rules {
		if rule == "dive" {
			return applyDiveValidationRules(schema, typ, rules[index+1:])
		}
		if rule == "keys" || rule == "endkeys" {
			return fmt.Errorf("%q must follow dive on a map", rule)
		}
		if rule == "" || rule == "omitempty" || rule == "omitzero" || rule == "omitnil" || rule == "structonly" || rule == "nostructlevel" {
			continue
		}
		if strings.Contains(rule, "|") {
			if err := applyOrValidationRule(schema, typ, rule); err != nil {
				return err
			}
			continue
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
				continue
			}
			addPatternConstraint(schema, pattern)
		case "unique":
			if ok && value != "" {
				continue // unique=Field has no direct JSON Schema equivalent.
			}
			if typ.Kind() == reflect.Array || typ.Kind() == reflect.Slice {
				schema.UniqueItems = true
			}
		case "email", "url", "uri", "hostname", "ipv4", "ipv6", "uuid", "uuid3", "uuid4", "uuid5", "datetime":
			if typ.Kind() != reflect.String {
				return fmt.Errorf("%s is only valid for string fields", name)
			}
			schema.Format = validatorJSONSchemaFormat(name)
		case "notblank":
			if typ.Kind() != reflect.String {
				return fmt.Errorf("notblank is only valid for string fields")
			}
			addPatternConstraint(schema, `\S`)
		default:
			// go-playground/validator remains the source of truth for runtime
			// validation. Rules without a direct JSON Schema equivalent are
			// intentionally left to it.
			continue
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
		branches = append(branches, branch)
	}
	schema.AllOf = append(schema.AllOf, &jsonschema.Schema{AnyOf: branches})
	return nil
}

func validatorJSONSchemaFormat(rule string) string {
	switch rule {
	case "url", "uri":
		return "uri"
	case "uuid3", "uuid4", "uuid5":
		return "uuid"
	case "datetime":
		return "date-time"
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
	default:
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
	default:
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
	}
}

func applyComparisonRule(schema *jsonschema.Schema, kind reflect.Kind, rule, raw string) error {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%s=%q is not numeric", rule, raw)
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
			return fmt.Errorf("%s=%q must be a non-negative integer", rule, raw)
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
	default:
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
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%s=%q is not numeric", rule, raw)
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
			setStrongestMinimum(minimum, value)
		case "max":
			setStrongestMaximum(maximum, value)
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
