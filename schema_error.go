package loom

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/google/jsonschema-go/jsonschema"
)

// ToolArgumentErrorKind identifies the stage that rejected tool arguments.
type ToolArgumentErrorKind string

const (
	ToolArgumentErrorMalformedJSON ToolArgumentErrorKind = "malformed_json"
	ToolArgumentErrorSchema        ToolArgumentErrorKind = "schema_validation"
	ToolArgumentErrorStruct        ToolArgumentErrorKind = "struct_validation"

	maxToolArgumentMessages     = 8
	maxToolArgumentMessageRunes = 512
	maxExpectedArgumentRunes    = 4096
	maxExampleArgumentRunes     = 4096
	// Issues carries structured diagnostics, and both its length and the text
	// inside it are shaped by model-supplied field names. Error() bounds what it
	// renders, but consumers that walk Issues themselves need the stored values
	// bounded too — otherwise a single absurd key can be fed straight back to a
	// model.
	maxToolArgumentIssues     = 64
	maxToolArgumentFieldRunes = 256
)

// ToolArgumentIssue is one model-facing validation problem.
type ToolArgumentIssue struct {
	Field   string
	Rule    string
	Message string
}

// ToolArgumentError is the normalized error returned for invalid tool input.
// Err retains the original parser, JSON Schema, or validator error for logs and
// errors.Is/errors.As. ExpectedArguments describes the accepted shape without
// looking like callable JSON. ExampleArguments is present only when declared
// examples form a complete, validated call.
type ToolArgumentError struct {
	Tool              string
	Kind              ToolArgumentErrorKind
	Issues            []ToolArgumentIssue
	ExpectedArguments string
	ExampleArguments  string
	Err               error
}

func (e *ToolArgumentError) Error() string {
	prefix := "invalid tool arguments"
	if e.Tool != "" {
		prefix = fmt.Sprintf("invalid arguments for tool %q", e.Tool)
	}
	rendered := summarizeUnknownFieldIssues(e.Issues)
	messages := make([]string, 0, min(len(rendered), maxToolArgumentMessages)+1)
	rendering := 0
	for _, issue := range rendered {
		if issue.Message == "" {
			continue
		}
		rendering++
		if len(messages) == maxToolArgumentMessages {
			continue
		}
		messages = append(messages, truncateDiagnostic(issue.Message, maxToolArgumentMessageRunes))
	}
	if remaining := rendering - len(messages); remaining > 0 {
		messages = append(messages, fmt.Sprintf("and %d more validation issues", remaining))
	}
	if len(messages) == 0 {
		messages = append(messages, "input does not match the tool contract")
	}
	message := prefix + ": " + strings.Join(messages, "; ")
	if e.ExpectedArguments != "" {
		message += ". expected arguments: " + truncateDiagnostic(e.ExpectedArguments, maxExpectedArgumentRunes)
	}
	if e.ExampleArguments != "" {
		if len([]rune(e.ExampleArguments)) <= maxExampleArgumentRunes {
			message += ". example arguments: " + e.ExampleArguments
		} else {
			message += ". example arguments omitted because the validated example is too large"
		}
	}
	return message
}

// summarizeUnknownFieldIssues collapses stray-field issues into one line for
// rendering. A model that guessed a dozen field names would otherwise spend the
// whole message budget being told about each one, and listing them together
// reads better than repeating the same sentence.
//
// The structured Issues slice keeps them separate; only the rendered text is
// condensed.
func summarizeUnknownFieldIssues(issues []ToolArgumentIssue) []ToolArgumentIssue {
	unknown := 0
	for _, issue := range issues {
		if issue.Rule == "unknown" {
			unknown++
		}
	}
	if unknown < 2 {
		return issues
	}
	out := make([]ToolArgumentIssue, 0, len(issues)-unknown+1)
	names := make([]string, 0, unknown)
	for _, issue := range issues {
		if issue.Rule != "unknown" {
			out = append(out, issue)
			continue
		}
		if issue.Field != "" {
			names = append(names, strconv.Quote(issue.Field))
		}
	}
	message := fmt.Sprintf("%d fields are not accepted", unknown)
	if len(names) > 0 {
		message = "these fields are not accepted: " + strings.Join(names, ", ")
	}
	return append(out, ToolArgumentIssue{Rule: "unknown", Message: message})
}

// clampIssues bounds diagnostics built from model-supplied input, so neither
// the number of issues nor the text inside any one of them can grow without
// limit before a consumer reads them.
func clampIssues(issues []ToolArgumentIssue) []ToolArgumentIssue {
	if len(issues) > maxToolArgumentIssues {
		issues = issues[:maxToolArgumentIssues]
	}
	for index := range issues {
		issues[index].Field = truncateDiagnostic(issues[index].Field, maxToolArgumentFieldRunes)
		issues[index].Message = truncateDiagnostic(issues[index].Message, maxToolArgumentMessageRunes)
	}
	return issues
}

func truncateDiagnostic(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum-1]) + "…"
}

func (e *ToolArgumentError) Unwrap() error { return e.Err }

func newJSONToolArgumentError(tool string, guidance argumentGuidance, err error) error {
	message := "malformed JSON: " + err.Error()
	if errors.Is(err, errMultipleJSONValues) {
		message = "input must contain exactly one JSON object"
	} else {
		if _, ok := errors.AsType[*unsupportedJSONNumberError](err); ok {
			message = "unsupported numeric value: " + err.Error()
		}
	}
	return &ToolArgumentError{
		Tool:              tool,
		Kind:              ToolArgumentErrorMalformedJSON,
		Issues:            []ToolArgumentIssue{{Rule: "json", Message: message}},
		ExpectedArguments: guidance.expected,
		ExampleArguments:  guidance.example,
		Err:               err,
	}
}

// newTypeMismatchToolArgumentError reports a value whose JSON type does not fit
// the argument field it decodes into. The document itself parsed cleanly, so
// calling it malformed would send the model hunting for a syntax error that is
// not there. Only the JSON field path is reported; Go type internals are not
// exposed to the model.
func newTypeMismatchToolArgumentError(tool string, guidance argumentGuidance, typeError *jsonv2.SemanticError) error {
	label := "arguments"
	field := strings.TrimPrefix(string(typeError.JSONPointer), "/")
	if field != "" {
		label = quoteField(field)
	}
	message := label + " must be " + jsonTypeNameForGoType(typeError.GoType)
	if typeError.JSONKind != jsontext.KindInvalid {
		message += ", but got " + typeError.JSONKind.String()
	}
	return &ToolArgumentError{
		Tool:              tool,
		Kind:              ToolArgumentErrorSchema,
		Issues:            clampIssues([]ToolArgumentIssue{{Field: field, Rule: "type", Message: message}}),
		ExpectedArguments: guidance.expected,
		ExampleArguments:  guidance.example,
		Err:               typeError,
	}
}

// jsonTypeNameForGoType names the JSON type a Go type accepts, so diagnostics
// speak the model's vocabulary rather than Go's.
func jsonTypeNameForGoType(typ reflect.Type) string {
	for typ != nil && (typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Interface) {
		if typ.Kind() == reflect.Interface {
			return "a JSON value"
		}
		typ = typ.Elem()
	}
	if typ == nil {
		return "a JSON value"
	}
	switch typ.Kind() {
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Slice, reflect.Array:
		return "an array"
	case reflect.Map, reflect.Struct:
		return "an object"
	case reflect.Invalid, reflect.Complex64, reflect.Complex128, reflect.Chan,
		reflect.Func, reflect.Interface, reflect.Pointer, reflect.UnsafePointer:
		// No JSON type describes these; the pointer and interface cases are
		// unwrapped above, and the rest cannot appear in a decodable argument.
		return "a JSON value"
	default:
		// Unreachable: every reflect.Kind is listed above.
		return "a JSON value"
	}
}

func newSchemaToolArgumentError(tool string, schema *jsonschema.Schema, guidance argumentGuidance, instance any, err error) error {
	return &ToolArgumentError{
		Tool:              tool,
		Kind:              ToolArgumentErrorSchema,
		Issues:            clampIssues(explainSchemaError(schema, instance, err)),
		ExpectedArguments: guidance.expected,
		ExampleArguments:  guidance.example,
		Err:               err,
	}
}

func newStructToolArgumentError(tool string, guidance argumentGuidance, err error) error {
	return &ToolArgumentError{
		Tool:              tool,
		Kind:              ToolArgumentErrorStruct,
		Issues:            clampIssues(explainValidatorError(err)),
		ExpectedArguments: guidance.expected,
		ExampleArguments:  guidance.example,
		Err:               err,
	}
}

func explainValidatorError(err error) []ToolArgumentIssue {
	validationErrors, ok := errors.AsType[validator.ValidationErrors](err)
	if !ok {
		return []ToolArgumentIssue{{Rule: "validate", Message: "tool argument validation is misconfigured"}}
	}
	issues := make([]ToolArgumentIssue, 0, len(validationErrors))
	for _, fieldError := range validationErrors {
		field := validatorFieldPath(fieldError)
		rule := fieldError.Tag()
		param := fieldError.Param()
		label := quoteField(field)
		message := ""
		switch rule {
		case "required":
			message = label + " is required"
		case "min":
			message = label + minimumMessage(fieldError.Kind(), param)
		case "max":
			message = label + maximumMessage(fieldError.Kind(), param)
		case "len":
			message = label + lengthMessage(fieldError.Kind(), param)
		case "gt":
			message = label + comparisonMessage(fieldError.Kind(), "greater than", param)
		case "gte":
			message = label + comparisonMessage(fieldError.Kind(), "at least", param)
		case "lt":
			message = label + comparisonMessage(fieldError.Kind(), "less than", param)
		case "lte":
			message = label + comparisonMessage(fieldError.Kind(), "at most", param)
		case "eq":
			message = label + equalityMessage(fieldError.Kind(), "equal", param)
		case "ne":
			message = label + equalityMessage(fieldError.Kind(), "not equal", param)
		case "oneof":
			message = label + " must be one of " + compactJSON(parseOneOfValues(param))
		case "contains":
			message = label + " must contain " + strconv.Quote(param)
		case "startswith":
			message = label + " must start with " + strconv.Quote(param)
		case "endswith":
			message = label + " must end with " + strconv.Quote(param)
		case "excludes":
			message = label + " must not contain " + strconv.Quote(param)
		case "unique":
			message = label + " must contain unique items"
		case "notblank":
			message = label + " must not be blank"
		case "datetime":
			// Naming the layout is the whole point: without it the model cannot
			// tell "2026-08-25" from "2026-08-25T10:00:00Z" and has nothing to
			// correct towards.
			if param != "" {
				message = label + " must be a datetime in layout " + strconv.Quote(param)
			} else {
				message = label + " must be a valid datetime"
			}
		case "url", "http_url", "https_url", "email", "uri", "hostname", "ipv4", "ipv6", "uuid", "uuid3", "uuid4", "uuid5":
			message = label + " must be a valid " + strings.ReplaceAll(rule, "_", " ")
		default:
			constraint := rule
			// For an or-rule the tag already reads "url|startswith=/" and Param
			// returns the winning alternative's argument; appending it again
			// would render "url|startswith=/=/".
			if param != "" && !strings.Contains(rule, "|") {
				constraint += "=" + param
			}
			message = label + " must satisfy " + strconv.Quote(constraint)
		}
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: rule, Message: message})
	}
	sortIssues(issues)
	return issues
}

// kindPhrasing picks how a bound reads for a given kind: strings are measured
// in characters, containers in items, and everything else is compared as a
// plain value. Every reflect.Kind is accounted for here so the five message
// helpers below stay one line each.
func kindPhrasing(kind reflect.Kind, characters, items, value string) string {
	switch kind {
	case reflect.String:
		return characters
	case reflect.Array, reflect.Slice, reflect.Map:
		return items
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.Invalid, reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Pointer, reflect.Struct, reflect.UnsafePointer:
		return value
	default:
		// Unreachable: every reflect.Kind is listed above.
		return value
	}
}

func comparisonMessage(kind reflect.Kind, comparison, param string) string {
	return kindPhrasing(kind,
		" must contain a number of characters "+comparison+" "+param,
		" must contain a number of items "+comparison+" "+param,
		" must be "+comparison+" "+param)
}

func equalityMessage(kind reflect.Kind, comparison, param string) string {
	// A string is compared as a value here, not by length, so it shares the
	// value wording — quoted, since the parameter is literal text.
	value := " must be " + comparison + " to " + strconv.Quote(param)
	return kindPhrasing(kind,
		value,
		" must contain a number of items "+comparison+" to "+param,
		value)
}

func validatorFieldPath(fieldError validator.FieldError) string {
	path := fieldError.Namespace()
	if _, rest, ok := strings.Cut(path, "."); ok {
		path = rest
	}
	if path == "" {
		path = fieldError.Field()
	}
	return path
}

func minimumMessage(kind reflect.Kind, param string) string {
	return kindPhrasing(kind,
		" must contain at least "+param+" characters",
		" must contain at least "+param+" items",
		" must be at least "+param)
}

func maximumMessage(kind reflect.Kind, param string) string {
	return kindPhrasing(kind,
		" must contain at most "+param+" characters",
		" must contain at most "+param+" items",
		" must be at most "+param)
}

func lengthMessage(kind reflect.Kind, param string) string {
	return kindPhrasing(kind,
		" must contain exactly "+param+" characters",
		" must contain exactly "+param+" items",
		" must equal "+param)
}

func explainSchemaError(schema *jsonschema.Schema, instance any, err error) []ToolArgumentIssue {
	issues := diagnoseSchema(schema, instance, "")
	if len(issues) == 0 {
		issues = []ToolArgumentIssue{{Rule: "schema", Message: "input does not match the expected schema"}}
	}
	sortIssues(issues)
	return issues
}

func diagnoseSchema(schema *jsonschema.Schema, value any, field string) []ToolArgumentIssue {
	if schema == nil {
		return nil
	}
	label := quoteField(field)
	if matches, want := matchesSchemaType(schema, value); !matches {
		return []ToolArgumentIssue{{Field: field, Rule: "type", Message: label + " must be " + want}}
	}

	var issues []ToolArgumentIssue
	if schema.Const != nil && !equalJSONValue(value, *schema.Const) {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "const", Message: label + " must equal " + compactJSON(*schema.Const)})
	}
	if schema.Enum != nil && !containsJSONValue(schema.Enum, value) {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "oneof", Message: label + " must be one of " + compactJSON(schema.Enum)})
	}
	if len(schema.AnyOf) > 0 && !anySchemaMatches(schema.AnyOf, value) {
		if alternatives, ok := constAlternatives(schema.AnyOf); ok {
			issues = append(issues, ToolArgumentIssue{Field: field, Rule: "oneof", Message: label + " must be one of " + compactJSON(alternatives)})
		} else {
			issues = append(issues, ToolArgumentIssue{Field: field, Rule: "anyof", Message: label + " must satisfy at least one allowed constraint"})
		}
	}
	issues = append(issues, diagnoseNotSchema(schema.Not, value, field)...)

	if number, ok := jsonNumericValue(value); ok {
		issues = append(issues, diagnoseNumberSchema(schema, number, field)...)
	} else {
		switch typed := value.(type) {
		case string:
			issues = append(issues, diagnoseStringSchema(schema, typed, field)...)
		case []any:
			issues = append(issues, diagnoseArraySchema(schema, typed, field)...)
		case map[string]any:
			issues = append(issues, diagnoseObjectSchema(schema, typed, field)...)
		}
	}
	for _, constraint := range schema.AllOf {
		issues = append(issues, diagnoseSchema(constraint, value, field)...)
	}
	return issues
}

func anySchemaMatches(schemas []*jsonschema.Schema, value any) bool {
	for _, schema := range schemas {
		if schema == nil {
			continue
		}
		resolved, err := schema.Resolve(nil)
		if err == nil && resolved.Validate(value) == nil {
			return true
		}
	}
	return false
}

func constAlternatives(schemas []*jsonschema.Schema) ([]any, bool) {
	values := make([]any, 0, len(schemas))
	for _, schema := range schemas {
		if schema == nil || schema.Const == nil {
			return nil, false
		}
		values = append(values, *schema.Const)
	}
	return values, true
}

func diagnoseNotSchema(schema *jsonschema.Schema, value any, field string) []ToolArgumentIssue {
	if schema == nil {
		return nil
	}
	label := quoteField(field)
	if schema.Const != nil && equalJSONValue(value, *schema.Const) {
		return []ToolArgumentIssue{{Field: field, Rule: "ne", Message: label + " must not equal " + compactJSON(*schema.Const)}}
	}
	if text, ok := value.(string); ok && schema.Pattern != "" {
		pattern, err := regexp.Compile(schema.Pattern)
		if err == nil && pattern.MatchString(text) {
			if literal, kind, ok := literalPatternConstraint(schema.Pattern); ok && kind == "contains" {
				return []ToolArgumentIssue{{Field: field, Rule: "excludes", Message: label + " must not contain " + strconv.Quote(literal)}}
			}
			return []ToolArgumentIssue{{Field: field, Rule: "not", Message: label + " must not match pattern " + strconv.Quote(schema.Pattern)}}
		}
	}
	return nil
}

func matchesSchemaType(schema *jsonschema.Schema, value any) (bool, string) {
	allowed := slices.Clone(schema.Types)
	if schema.Type != "" {
		allowed = []string{schema.Type}
	}
	if len(allowed) == 0 {
		return true, "value"
	}
	actual := jsonValueType(value)
	for _, want := range allowed {
		if actual == want || (actual == "integer" && want == "number") {
			return true, schemaTypeName(schema)
		}
	}
	return false, schemaTypeName(schema)
}

func jsonValueType(value any) string {
	if number, ok := jsonNumericValue(value); ok {
		if number.IsInt() {
			return "integer"
		}
		return "number"
	}
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

func diagnoseStringSchema(schema *jsonschema.Schema, value, field string) []ToolArgumentIssue {
	label := quoteField(field)
	length := len([]rune(value))
	var issues []ToolArgumentIssue
	if schema.MinLength != nil && length < *schema.MinLength {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "min", Message: label + " must contain at least " + strconv.Itoa(*schema.MinLength) + " characters"})
	}
	if schema.MaxLength != nil && length > *schema.MaxLength {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "max", Message: label + " must contain at most " + strconv.Itoa(*schema.MaxLength) + " characters"})
	}
	if schema.Pattern != "" {
		pattern, err := regexp.Compile(schema.Pattern)
		if err == nil && !pattern.MatchString(value) {
			if schema.Pattern == `\S` {
				issues = append(issues, ToolArgumentIssue{Field: field, Rule: "notblank", Message: label + " must not be blank"})
			} else if literal, kind, ok := literalPatternConstraint(schema.Pattern); ok {
				switch kind {
				case "startswith":
					issues = append(issues, ToolArgumentIssue{Field: field, Rule: kind, Message: label + " must start with " + strconv.Quote(literal)})
				case "endswith":
					issues = append(issues, ToolArgumentIssue{Field: field, Rule: kind, Message: label + " must end with " + strconv.Quote(literal)})
				default:
					issues = append(issues, ToolArgumentIssue{Field: field, Rule: kind, Message: label + " must contain " + strconv.Quote(literal)})
				}
			} else {
				issues = append(issues, ToolArgumentIssue{Field: field, Rule: "pattern", Message: label + " must match pattern " + strconv.Quote(schema.Pattern)})
			}
		}
	}
	return issues
}

func literalPatternConstraint(pattern string) (literal, kind string, ok bool) {
	kind = "contains"
	if strings.HasPrefix(pattern, "^") {
		kind = "startswith"
		pattern = strings.TrimPrefix(pattern, "^")
	}
	if strings.HasSuffix(pattern, "$") {
		if kind != "contains" {
			return "", "", false
		}
		kind = "endswith"
		pattern = strings.TrimSuffix(pattern, "$")
	}
	literal = unquoteRegexpLiteral(pattern)
	return literal, kind, regexp.QuoteMeta(literal) == pattern
}

func unquoteRegexpLiteral(pattern string) string {
	var b strings.Builder
	for len(pattern) > 0 {
		if pattern[0] == '\\' && len(pattern) > 1 && strings.ContainsRune(`\.+*?()|[]{}^$`, rune(pattern[1])) {
			b.WriteByte(pattern[1])
			pattern = pattern[2:]
			continue
		}
		b.WriteByte(pattern[0])
		pattern = pattern[1:]
	}
	return b.String()
}

func diagnoseNumberSchema(schema *jsonschema.Schema, value *big.Rat, field string) []ToolArgumentIssue {
	label := quoteField(field)
	var issues []ToolArgumentIssue
	compare := func(bound float64) int { return value.Cmp(new(big.Rat).SetFloat64(bound)) }
	if schema.Minimum != nil && compare(*schema.Minimum) < 0 {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "min", Message: label + " must be at least " + formatNumber(*schema.Minimum)})
	}
	if schema.Maximum != nil && compare(*schema.Maximum) > 0 {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "max", Message: label + " must be at most " + formatNumber(*schema.Maximum)})
	}
	if schema.ExclusiveMinimum != nil && compare(*schema.ExclusiveMinimum) <= 0 {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "gt", Message: label + " must be greater than " + formatNumber(*schema.ExclusiveMinimum)})
	}
	if schema.ExclusiveMaximum != nil && compare(*schema.ExclusiveMaximum) >= 0 {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "lt", Message: label + " must be less than " + formatNumber(*schema.ExclusiveMaximum)})
	}
	if schema.MultipleOf != nil && *schema.MultipleOf != 0 {
		divisor := new(big.Rat).SetFloat64(*schema.MultipleOf)
		if quotient := new(big.Rat).Quo(value, divisor); !quotient.IsInt() {
			issues = append(issues, ToolArgumentIssue{Field: field, Rule: "multipleof", Message: label + " must be a multiple of " + formatNumber(*schema.MultipleOf)})
		}
	}
	return issues
}

func diagnoseArraySchema(schema *jsonschema.Schema, value []any, field string) []ToolArgumentIssue {
	label := quoteField(field)
	var issues []ToolArgumentIssue
	if schema.MinItems != nil && len(value) < *schema.MinItems {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "min", Message: label + " must contain at least " + strconv.Itoa(*schema.MinItems) + " items"})
	}
	if schema.MaxItems != nil && len(value) > *schema.MaxItems {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "max", Message: label + " must contain at most " + strconv.Itoa(*schema.MaxItems) + " items"})
	}
	if schema.UniqueItems {
		// Keyed lookup rather than pairwise comparison: array contents come
		// straight from the model, and a quadratic scan here burns seconds of
		// CPU on a few thousand items — on a path that only runs once
		// validation has already failed.
		seen := make(map[string]struct{}, len(value))
		for _, item := range value {
			key := canonicalJSONKey(item)
			if _, exists := seen[key]; exists {
				issues = append(issues, ToolArgumentIssue{Field: field, Rule: "unique", Message: label + " must contain unique items"})
				break
			}
			seen[key] = struct{}{}
		}
	}
	if schema.Items != nil {
		for index, item := range value {
			issues = append(issues, diagnoseSchema(schema.Items, item, indexFieldPath(field, index))...)
		}
	}
	return issues
}

func diagnoseObjectSchema(schema *jsonschema.Schema, value map[string]any, field string) []ToolArgumentIssue {
	label := quoteField(field)
	var issues []ToolArgumentIssue
	if schema.MinProperties != nil && len(value) < *schema.MinProperties {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "min", Message: label + " must contain at least " + strconv.Itoa(*schema.MinProperties) + " fields"})
	}
	if schema.MaxProperties != nil && len(value) > *schema.MaxProperties {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "max", Message: label + " must contain at most " + strconv.Itoa(*schema.MaxProperties) + " fields"})
	}
	for _, name := range schema.Required {
		if _, exists := value[name]; !exists {
			path := joinFieldPath(field, name)
			issues = append(issues, ToolArgumentIssue{Field: path, Rule: "required", Message: quoteField(path) + " is required"})
		}
	}
	for _, name := range orderedPropertyNames(schema) {
		if propertyValue, exists := value[name]; exists {
			issues = append(issues, diagnoseSchema(schema.Properties[name], propertyValue, joinFieldPath(field, name))...)
		}
	}
	for name, propertyValue := range value {
		if schema.PropertyNames != nil {
			// Key constraints are relabelled: rendered on the same path as the
			// value they belong to, "m.ab must contain at least 3 characters"
			// gives the model no way to tell whether the key or the value is
			// wrong.
			keyPath := joinFieldPath(field, name)
			for _, issue := range diagnoseSchema(schema.PropertyNames, name, keyPath) {
				issue.Message = "field name " + strconv.Quote(name) + " in " + label +
					strings.TrimPrefix(issue.Message, quoteField(keyPath))
				issues = append(issues, issue)
			}
		}
		if _, exists := schema.Properties[name]; exists {
			continue
		}
		path := joinFieldPath(field, name)
		switch {
		case isFalseSchema(schema.AdditionalProperties):
			issues = append(issues, ToolArgumentIssue{Field: path, Rule: "unknown", Message: quoteField(path) + " is not an accepted field"})
		case schema.AdditionalProperties != nil:
			issues = append(issues, diagnoseSchema(schema.AdditionalProperties, propertyValue, path)...)
		}
	}
	return issues
}

func orderedPropertyNames(schema *jsonschema.Schema) []string {
	names := slices.Clone(schema.PropertyOrder)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	var remaining []string
	for name := range schema.Properties {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(names, remaining...)
}

func isFalseSchema(schema *jsonschema.Schema) bool {
	return schema != nil && schema.Not != nil && reflect.ValueOf(*schema.Not).IsZero()
}

func containsJSONValue(values []any, target any) bool {
	for _, value := range values {
		if equalJSONValue(value, target) {
			return true
		}
	}
	return false
}

// canonicalJSONKey builds a lookup key matching equalJSONValue semantics:
// numerically equal values share a key regardless of their Go type or literal
// form (1, 1.0 and json.Number("1")), and object members are ordered so member
// order never affects the key.
func canonicalJSONKey(value any) string {
	var builder strings.Builder
	writeCanonicalJSONKey(&builder, value)
	return builder.String()
}

func writeCanonicalJSONKey(builder *strings.Builder, value any) {
	if rational, ok := jsonNumericValue(value); ok {
		builder.WriteString("#n:")
		builder.WriteString(rational.RatString())
		return
	}
	switch typed := value.(type) {
	case nil:
		builder.WriteString("#z")
	case bool:
		builder.WriteString("#b:")
		builder.WriteString(strconv.FormatBool(typed))
	case string:
		builder.WriteString("#s:")
		builder.WriteString(strconv.Quote(typed))
	case []any:
		builder.WriteString("#a[")
		for _, item := range typed {
			writeCanonicalJSONKey(builder, item)
			builder.WriteByte(',')
		}
		builder.WriteByte(']')
	case map[string]any:
		names := make([]string, 0, len(typed))
		for name := range typed {
			names = append(names, name)
		}
		sort.Strings(names)
		builder.WriteString("#o{")
		for _, name := range names {
			builder.WriteString(strconv.Quote(name))
			builder.WriteByte(':')
			writeCanonicalJSONKey(builder, typed[name])
			builder.WriteByte(',')
		}
		builder.WriteByte('}')
	default:
		// Unknown dynamic type: fall back to a type-qualified rendering so
		// distinct values never collide into one key.
		fmt.Fprintf(builder, "#?%T:%#v", typed, typed)
	}
}

func equalJSONValue(left, right any) bool {
	leftNumber, leftOK := jsonNumericValue(left)
	rightNumber, rightOK := jsonNumericValue(right)
	if leftOK && rightOK {
		return leftNumber.Cmp(rightNumber) == 0
	}
	return reflect.DeepEqual(left, right)
}

func jsonNumericValue(value any) (*big.Rat, bool) {
	rational := new(big.Rat)
	switch value := value.(type) {
	case rawJSONNumber:
		if _, ok := rational.SetString(string(value)); !ok {
			return nil, false
		}
		return rational, true
	case float64:
		return rational.SetFloat64(value), true
	case float32:
		return rational.SetFloat64(float64(value)), true
	case int:
		return rational.SetInt64(int64(value)), true
	case int8:
		return rational.SetInt64(int64(value)), true
	case int16:
		return rational.SetInt64(int64(value)), true
	case int32:
		return rational.SetInt64(int64(value)), true
	case int64:
		return rational.SetInt64(value), true
	case uint:
		return rational.SetUint64(uint64(value)), true
	case uint8:
		return rational.SetUint64(uint64(value)), true
	case uint16:
		return rational.SetUint64(uint64(value)), true
	case uint32:
		return rational.SetUint64(uint64(value)), true
	case uint64:
		return rational.SetUint64(value), true
	default:
		return nil, false
	}
}

func indexFieldPath(prefix string, index int) string {
	return prefix + "[" + strconv.Itoa(index) + "]"
}

func joinFieldPath(prefix, field string) string {
	if prefix == "" {
		return field
	}
	return prefix + "." + field
}

// sortIssues orders diagnostics by how useful they are to a model, then by
// field for stability. Only the first few issues survive rendering, and a model
// that invented twenty stray fields would otherwise push the one actionable
// line — a missing required field — out of view.
func sortIssues(issues []ToolArgumentIssue) {
	sort.SliceStable(issues, func(i, j int) bool {
		leftRank, rightRank := issueRank(issues[i].Rule), issueRank(issues[j].Rule)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if issues[i].Field == issues[j].Field {
			return issues[i].Rule < issues[j].Rule
		}
		return issues[i].Field < issues[j].Field
	})
}

func issueRank(rule string) int {
	switch rule {
	case "required":
		return 0
	case "type":
		return 1
	case "unknown":
		// Stray fields are the least actionable: the fix is to drop them, and
		// they are summarized as a group when rendered.
		return 3
	default:
		return 2
	}
}

func quoteField(field string) string {
	if field == "" {
		return "input"
	}
	return strconv.Quote(field)
}

func summarizeExpectedArguments(schema *jsonschema.Schema) string {
	if schema == nil || !schemaHasType(schema, "object") {
		return ""
	}
	names := slices.Clone(schema.PropertyOrder)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	var remaining []string
	for name := range schema.Properties {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	names = append(names, remaining...)
	if len(names) == 0 {
		// Saying outright that the tool takes nothing beats making the model
		// infer it from an empty example.
		return "none"
	}

	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(name)
		b.WriteString("=<")
		b.WriteString(summarizeProperty(schema.Properties[name], slices.Contains(schema.Required, name)))
		b.WriteByte('>')
	}
	return b.String()
}

func summarizeProperty(schema *jsonschema.Schema, required bool) string {
	parts := []string{schemaTypeName(schema)}
	if required {
		parts = append(parts, "required")
	} else {
		parts = append(parts, "optional")
	}
	if schema == nil {
		return strings.Join(parts, ", ")
	}
	parts = appendSchemaConstraintParts(parts, schema)
	return strings.Join(parts, ", ")
}

func appendSchemaConstraintParts(parts []string, schema *jsonschema.Schema) []string {
	if schema == nil {
		return parts
	}
	if schema.Const != nil {
		parts = append(parts, "equals "+compactJSON(*schema.Const))
	}
	if schema.Not != nil && schema.Not.Const != nil {
		parts = append(parts, "not "+compactJSON(*schema.Not.Const))
	}
	if schema.Enum != nil {
		parts = append(parts, "one of "+compactJSON(schema.Enum))
	}
	if alternatives, ok := constAlternatives(schema.AnyOf); ok && len(alternatives) > 0 {
		parts = append(parts, "one of "+compactJSON(alternatives))
	}
	if schema.Format != "" {
		parts = append(parts, "format "+schema.Format)
	}
	if schema.Minimum != nil || schema.Maximum != nil {
		switch {
		case schema.Minimum != nil && schema.Maximum != nil:
			parts = append(parts, formatNumber(*schema.Minimum)+".."+formatNumber(*schema.Maximum))
		case schema.Minimum != nil:
			parts = append(parts, ">="+formatNumber(*schema.Minimum))
		case schema.Maximum != nil:
			parts = append(parts, "<="+formatNumber(*schema.Maximum))
		}
	}
	if schema.ExclusiveMinimum != nil {
		parts = append(parts, ">"+formatNumber(*schema.ExclusiveMinimum))
	}
	if schema.ExclusiveMaximum != nil {
		parts = append(parts, "<"+formatNumber(*schema.ExclusiveMaximum))
	}
	if schema.MinLength != nil {
		parts = append(parts, "min length "+strconv.Itoa(*schema.MinLength))
	}
	if schema.MaxLength != nil {
		parts = append(parts, "max length "+strconv.Itoa(*schema.MaxLength))
	}
	if schema.Pattern != "" {
		if schema.Pattern == `\S` {
			parts = append(parts, "non-blank")
		} else if literal, kind, ok := literalPatternConstraint(schema.Pattern); ok {
			parts = append(parts, strings.ReplaceAll(kind, "with", " with ")+" "+strconv.Quote(literal))
		} else {
			parts = append(parts, "pattern "+strconv.Quote(schema.Pattern))
		}
	}
	if schema.Not != nil && schema.Not.Pattern != "" {
		if literal, _, ok := literalPatternConstraint(schema.Not.Pattern); ok {
			parts = append(parts, "excludes "+strconv.Quote(literal))
		}
	}
	if schema.MinItems != nil {
		parts = append(parts, "min items "+strconv.Itoa(*schema.MinItems))
	}
	if schema.MaxItems != nil {
		parts = append(parts, "max items "+strconv.Itoa(*schema.MaxItems))
	}
	if schema.UniqueItems {
		parts = append(parts, "unique items")
	}
	if schema.MinProperties != nil {
		parts = append(parts, "min fields "+strconv.Itoa(*schema.MinProperties))
	}
	if schema.MaxProperties != nil {
		parts = append(parts, "max fields "+strconv.Itoa(*schema.MaxProperties))
	}
	for _, constraint := range schema.AllOf {
		parts = appendSchemaConstraintParts(parts, constraint)
	}
	return parts
}

func schemaTypeName(schema *jsonschema.Schema) string {
	if schema == nil {
		return "value"
	}
	if schema.Type != "" {
		return schema.Type
	}
	types := slices.DeleteFunc(slices.Clone(schema.Types), func(value string) bool { return value == "null" })
	if len(types) == 0 {
		return "value"
	}
	return strings.Join(types, " or ")
}

func formatNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func compactJSON(value any) string {
	data, err := jsonv2.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(data)
}
