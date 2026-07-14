package loom

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	messages := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		if issue.Message != "" {
			messages = append(messages, issue.Message)
		}
	}
	if len(messages) == 0 {
		messages = append(messages, "input does not match the tool contract")
	}
	message := prefix + ": " + strings.Join(messages, "; ")
	if e.ExpectedArguments != "" {
		message += ". expected arguments: " + e.ExpectedArguments
	}
	if e.ExampleArguments != "" {
		message += ". example arguments: " + e.ExampleArguments
	}
	return message
}

func (e *ToolArgumentError) Unwrap() error { return e.Err }

func newJSONToolArgumentError(tool string, guidance argumentGuidance, err error) error {
	message := "malformed JSON: " + err.Error()
	if errors.Is(err, errMultipleJSONValues) {
		message = "input must contain exactly one JSON object"
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

func newSchemaToolArgumentError(tool string, schema *jsonschema.Schema, guidance argumentGuidance, instance any, err error) error {
	return &ToolArgumentError{
		Tool:              tool,
		Kind:              ToolArgumentErrorSchema,
		Issues:            explainSchemaError(schema, instance, err),
		ExpectedArguments: guidance.expected,
		ExampleArguments:  guidance.example,
		Err:               err,
	}
}

func newStructToolArgumentError(tool string, guidance argumentGuidance, err error) error {
	return &ToolArgumentError{
		Tool:              tool,
		Kind:              ToolArgumentErrorStruct,
		Issues:            explainValidatorError(err),
		ExpectedArguments: guidance.expected,
		ExampleArguments:  guidance.example,
		Err:               err,
	}
}

func explainValidatorError(err error) []ToolArgumentIssue {
	var validationErrors validator.ValidationErrors
	if !errors.As(err, &validationErrors) {
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
		case "oneof":
			message = label + " must be one of [" + strings.Join(strings.Fields(param), ", ") + "]"
		case "contains":
			message = label + " must contain " + strconv.Quote(param)
		case "notblank":
			message = label + " must not be blank"
		case "url", "http_url", "https_url":
			message = label + " must be a valid " + strings.ReplaceAll(rule, "_", " ")
		default:
			constraint := rule
			if param != "" {
				constraint += "=" + param
			}
			message = label + " must satisfy " + strconv.Quote(constraint)
		}
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: rule, Message: message})
	}
	sortIssues(issues)
	return issues
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
	switch kind {
	case reflect.String:
		return " must contain at least " + param + " characters"
	case reflect.Array, reflect.Slice, reflect.Map:
		return " must contain at least " + param + " items"
	default:
		return " must be at least " + param
	}
}

func maximumMessage(kind reflect.Kind, param string) string {
	switch kind {
	case reflect.String:
		return " must contain at most " + param + " characters"
	case reflect.Array, reflect.Slice, reflect.Map:
		return " must contain at most " + param + " items"
	default:
		return " must be at most " + param
	}
}

func lengthMessage(kind reflect.Kind, param string) string {
	switch kind {
	case reflect.String:
		return " must contain exactly " + param + " characters"
	case reflect.Array, reflect.Slice, reflect.Map:
		return " must contain exactly " + param + " items"
	default:
		return " must equal " + param
	}
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

	switch typed := value.(type) {
	case string:
		issues = append(issues, diagnoseStringSchema(schema, typed, field)...)
	case float64:
		issues = append(issues, diagnoseNumberSchema(schema, typed, field)...)
	case []any:
		issues = append(issues, diagnoseArraySchema(schema, typed, field)...)
	case map[string]any:
		issues = append(issues, diagnoseObjectSchema(schema, typed, field)...)
	}
	return issues
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
	switch value := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if math.Trunc(value) == value {
			return "integer"
		}
		return "number"
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
			} else {
				issues = append(issues, ToolArgumentIssue{Field: field, Rule: "pattern", Message: label + " must match pattern " + strconv.Quote(schema.Pattern)})
			}
		}
	}
	return issues
}

func diagnoseNumberSchema(schema *jsonschema.Schema, value float64, field string) []ToolArgumentIssue {
	label := quoteField(field)
	var issues []ToolArgumentIssue
	if schema.Minimum != nil && value < *schema.Minimum {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "min", Message: label + " must be at least " + formatNumber(*schema.Minimum)})
	}
	if schema.Maximum != nil && value > *schema.Maximum {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "max", Message: label + " must be at most " + formatNumber(*schema.Maximum)})
	}
	if schema.ExclusiveMinimum != nil && value <= *schema.ExclusiveMinimum {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "gt", Message: label + " must be greater than " + formatNumber(*schema.ExclusiveMinimum)})
	}
	if schema.ExclusiveMaximum != nil && value >= *schema.ExclusiveMaximum {
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "lt", Message: label + " must be less than " + formatNumber(*schema.ExclusiveMaximum)})
	}
	if schema.MultipleOf != nil && *schema.MultipleOf != 0 {
		quotient := value / *schema.MultipleOf
		if math.Abs(quotient-math.Round(quotient)) > 1e-9 {
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
		duplicate := false
		for i := range value {
			for j := 0; j < i; j++ {
				if equalJSONValue(value[i], value[j]) {
					issues = append(issues, ToolArgumentIssue{Field: field, Rule: "unique", Message: label + " must contain unique items"})
					duplicate = true
					break
				}
			}
			if duplicate {
				break
			}
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

func equalJSONValue(left, right any) bool {
	leftNumber, leftOK := jsonNumericValue(left)
	rightNumber, rightOK := jsonNumericValue(right)
	if leftOK && rightOK {
		return leftNumber == rightNumber
	}
	return reflect.DeepEqual(left, right)
}

func jsonNumericValue(value any) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int8:
		return float64(value), true
	case int16:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint8:
		return float64(value), true
	case uint16:
		return float64(value), true
	case uint32:
		return float64(value), true
	case uint64:
		return float64(value), true
	default:
		return 0, false
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

func sortIssues(issues []ToolArgumentIssue) {
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Field == issues[j].Field {
			return issues[i].Rule < issues[j].Rule
		}
		return issues[i].Field < issues[j].Field
	})
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
	if schema.Enum != nil {
		parts = append(parts, "one of "+compactJSON(schema.Enum))
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
	if schema.MinLength != nil {
		parts = append(parts, "min length "+strconv.Itoa(*schema.MinLength))
	}
	if schema.MaxLength != nil {
		parts = append(parts, "max length "+strconv.Itoa(*schema.MaxLength))
	}
	if schema.Pattern == `\S` {
		parts = append(parts, "non-blank")
	}
	if schema.MinItems != nil {
		parts = append(parts, "min items "+strconv.Itoa(*schema.MinItems))
	}
	if schema.MaxItems != nil {
		parts = append(parts, "max items "+strconv.Itoa(*schema.MaxItems))
	}
	return strings.Join(parts, ", ")
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
	data, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(data)
}
