package loom

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
// errors.Is/errors.As. ExpectedInput is a compact JSON object describing the
// accepted shape; it deliberately omits long field descriptions.
type ToolArgumentError struct {
	Tool          string
	Kind          ToolArgumentErrorKind
	Issues        []ToolArgumentIssue
	ExpectedInput string
	Err           error
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
	if e.ExpectedInput != "" {
		message += ". expected input: " + e.ExpectedInput
	}
	return message
}

func (e *ToolArgumentError) Unwrap() error { return e.Err }

func newJSONToolArgumentError(tool, expected string, err error) error {
	message := "malformed JSON: " + err.Error()
	if strings.Contains(err.Error(), "multiple JSON values") {
		message = "input must contain exactly one JSON object"
	}
	return &ToolArgumentError{
		Tool:          tool,
		Kind:          ToolArgumentErrorMalformedJSON,
		Issues:        []ToolArgumentIssue{{Rule: "json", Message: message}},
		ExpectedInput: expected,
		Err:           err,
	}
}

func newSchemaToolArgumentError(tool string, schema *jsonschema.Schema, expected string, instance any, err error) error {
	return &ToolArgumentError{
		Tool:          tool,
		Kind:          ToolArgumentErrorSchema,
		Issues:        explainSchemaError(schema, instance, err),
		ExpectedInput: expected,
		Err:           err,
	}
}

func newStructToolArgumentError(tool, expected string, err error) error {
	return &ToolArgumentError{
		Tool:          tool,
		Kind:          ToolArgumentErrorStruct,
		Issues:        explainValidatorError(err),
		ExpectedInput: expected,
		Err:           err,
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
	raw := err.Error()
	path := schemaErrorPath(raw)
	field := strings.Join(path, ".")
	label := quoteField(field)
	targetSchema := schemaAtPath(schema, path)
	targetValue := valueAtPath(instance, path)

	switch {
	case strings.Contains(raw, "required: missing properties"):
		issues := missingRequiredIssues(targetSchema, targetValue, field)
		if len(issues) > 0 {
			return issues
		}
	case strings.Contains(raw, "unexpected additional properties"):
		issues := additionalPropertyIssues(targetSchema, targetValue, field)
		if len(issues) > 0 {
			return issues
		}
	case strings.Contains(raw, "maximum:") && targetSchema != nil && targetSchema.Maximum != nil:
		return []ToolArgumentIssue{{Field: field, Rule: "max", Message: label + " must be at most " + formatNumber(*targetSchema.Maximum)}}
	case strings.Contains(raw, "minimum:") && targetSchema != nil && targetSchema.Minimum != nil:
		return []ToolArgumentIssue{{Field: field, Rule: "min", Message: label + " must be at least " + formatNumber(*targetSchema.Minimum)}}
	case strings.Contains(raw, "maxLength:") && targetSchema != nil && targetSchema.MaxLength != nil:
		return []ToolArgumentIssue{{Field: field, Rule: "max", Message: label + " must contain at most " + strconv.Itoa(*targetSchema.MaxLength) + " characters"}}
	case strings.Contains(raw, "minLength:") && targetSchema != nil && targetSchema.MinLength != nil:
		return []ToolArgumentIssue{{Field: field, Rule: "min", Message: label + " must contain at least " + strconv.Itoa(*targetSchema.MinLength) + " characters"}}
	case strings.Contains(raw, "enum:") && targetSchema != nil:
		return []ToolArgumentIssue{{Field: field, Rule: "oneof", Message: label + " must be one of " + compactJSON(targetSchema.Enum)}}
	case strings.Contains(raw, "type:") && targetSchema != nil:
		return []ToolArgumentIssue{{Field: field, Rule: "type", Message: label + " must be " + schemaTypeName(targetSchema)}}
	case strings.Contains(raw, "pattern:") && targetSchema != nil && targetSchema.Pattern == `\S`:
		return []ToolArgumentIssue{{Field: field, Rule: "notblank", Message: label + " must not be blank"}}
	}
	return []ToolArgumentIssue{{Field: field, Rule: "schema", Message: label + " does not match the expected schema"}}
}

func schemaErrorPath(raw string) []string {
	const marker = "/properties/"
	var path []string
	for {
		_, rest, ok := strings.Cut(raw, marker)
		if !ok {
			return path
		}
		name := rest
		if i := strings.IndexAny(name, "/:"); i >= 0 {
			name = name[:i]
		}
		name = strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
		path = append(path, name)
		raw = rest
	}
}

func schemaAtPath(schema *jsonschema.Schema, path []string) *jsonschema.Schema {
	for _, part := range path {
		if schema == nil {
			return nil
		}
		schema = schema.Properties[part]
	}
	return schema
}

func valueAtPath(value any, path []string) any {
	for _, part := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[part]
	}
	return value
}

func missingRequiredIssues(schema *jsonschema.Schema, value any, prefix string) []ToolArgumentIssue {
	object, ok := value.(map[string]any)
	if !ok || schema == nil {
		return nil
	}
	var issues []ToolArgumentIssue
	for _, name := range schema.Required {
		if _, exists := object[name]; exists {
			continue
		}
		field := joinFieldPath(prefix, name)
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "required", Message: quoteField(field) + " is required"})
	}
	sortIssues(issues)
	return issues
}

func additionalPropertyIssues(schema *jsonschema.Schema, value any, prefix string) []ToolArgumentIssue {
	object, ok := value.(map[string]any)
	if !ok || schema == nil {
		return nil
	}
	var issues []ToolArgumentIssue
	for name := range object {
		if _, exists := schema.Properties[name]; exists {
			continue
		}
		field := joinFieldPath(prefix, name)
		issues = append(issues, ToolArgumentIssue{Field: field, Rule: "unknown", Message: quoteField(field) + " is not an accepted field"})
	}
	sortIssues(issues)
	return issues
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

func summarizeExpectedInput(schema *jsonschema.Schema) string {
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
	b.WriteByte('{')
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(name))
		b.WriteByte(':')
		b.WriteString(strconv.Quote(summarizeProperty(schema.Properties[name], slices.Contains(schema.Required, name))))
	}
	b.WriteByte('}')
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
		return strings.Join(parts, "; ")
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
	return strings.Join(parts, "; ")
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
