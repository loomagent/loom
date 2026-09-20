package loom

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/loomagent/loom/internal/toolcontract"
)

// ToolArgumentErrorKind identifies the stage that rejected tool arguments.
type ToolArgumentErrorKind string

const (
	ToolArgumentErrorMalformedJSON ToolArgumentErrorKind = "malformed_json"
	ToolArgumentErrorSchema        ToolArgumentErrorKind = "schema_validation"

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
	Code    string
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

func newSchemaToolArgumentError(tool string, guidance argumentGuidance, err *toolcontract.ValidationError) error {
	return &ToolArgumentError{
		Tool:              tool,
		Kind:              ToolArgumentErrorSchema,
		Issues:            clampIssues(explainValidationResult(err)),
		ExpectedArguments: guidance.expected,
		ExampleArguments:  guidance.example,
		Err:               err,
	}
}

func explainValidationResult(err *toolcontract.ValidationError) []ToolArgumentIssue {
	if err == nil {
		return []ToolArgumentIssue{{Rule: "schema", Message: "input does not match the expected schema"}}
	}
	issues := make([]ToolArgumentIssue, 0, len(err.Violations))
	collectValidationIssues(err.Violations, &issues)
	if len(issues) == 0 {
		issues = append(issues, ToolArgumentIssue{Rule: "schema", Message: "input does not match the expected schema"})
	}
	sortIssues(issues)
	return issues
}

func collectValidationIssues(violations []toolcontract.Violation, issues *[]ToolArgumentIssue) {
	for index, violation := range violations {
		field := violation.Field()
		if violation.Code == "any_of_item_mismatch" {
			if allowed := validationAlternatives(violations[index+1:], field); len(allowed) > 0 {
				*issues = append(*issues, ToolArgumentIssue{
					Field:   field,
					Rule:    "oneof",
					Code:    violation.Code,
					Message: quoteField(field) + " must be one of " + compactJSON(allowed),
				})
				continue
			}
		}
		if issue, ok := validationIssue(field, violation); ok {
			*issues = append(*issues, issue)
		}
	}
}

func validationAlternatives(violations []toolcontract.Violation, field string) []any {
	var alternatives []any
	for _, violation := range violations {
		if violation.Field() != field {
			break
		}
		if violation.Code == "const_mismatch" || violation.Code == "const_mismatch_null" {
			alternatives = append(alternatives, violation.Params["expected"])
		}
	}
	return alternatives
}

func validationIssue(field string, violation toolcontract.Violation) (ToolArgumentIssue, bool) {
	label := quoteField(field)
	rule := violation.Keyword
	code := violation.Code
	params := violation.Params
	message := ""
	switch code {
	case "property_mismatch", "properties_mismatch", "all_of_item_mismatch",
		"any_of_item_mismatch", "one_of_item_mismatch", "false_schema_mismatch":
		return ToolArgumentIssue{}, false
	case "missing_required_property":
		property := strings.Trim(fmt.Sprint(params["property"]), "'")
		field = joinField(field, property)
		message = quoteField(field) + " is required"
	case "missing_required_properties":
		message = label + " is missing required fields: " + fmt.Sprint(params["properties"])
	case "additional_property_mismatch", "additional_property_false":
		property := strings.Trim(fmt.Sprint(params["property"]), "'")
		field = joinField(field, property)
		rule = "unknown"
		message = quoteField(field) + " is not an accepted field"
	case "additional_properties_mismatch":
		rule = "unknown"
		message = "these fields are not accepted: " + fmt.Sprint(params["properties"])
	case "value_above_maximum":
		message = label + " must be at most " + fmt.Sprint(params["maximum"])
	case "value_below_minimum":
		message = label + " must be at least " + fmt.Sprint(params["minimum"])
	case "exclusive_maximum_mismatch":
		message = label + " must be less than " + fmt.Sprint(params["exclusive_maximum"])
	case "exclusive_minimum_mismatch":
		message = label + " must be greater than " + fmt.Sprint(params["exclusive_minimum"])
	case "value_not_in_enum":
		rule = "oneof"
		message = label + " must be one of " + compactJSON(params["allowed"])
	case "const_mismatch", "const_mismatch_null":
		message = label + " must equal " + compactJSON(params["expected"])
	case "unique_items_mismatch":
		message = label + " must contain unique items"
	case "pattern_mismatch":
		message = patternValidationMessage(label, fmt.Sprint(params["pattern"]))
	case "property_name_mismatch":
		property := strings.Trim(fmt.Sprint(params["property"]), "'")
		field = property
		message = "field name " + strconv.Quote(property) + " is not accepted"
	case "property_names_mismatch":
		message = "field names are not accepted: " + fmt.Sprint(params["properties"])
	case "string_too_short":
		message = label + " must contain at least " + fmt.Sprint(params["min_length"]) + " characters"
	case "string_too_long":
		message = label + " must contain at most " + fmt.Sprint(params["max_length"]) + " characters"
	case "items_too_short":
		message = label + " must contain at least " + fmt.Sprint(params["min_items"]) + " items"
	case "items_too_long":
		message = label + " must contain at most " + fmt.Sprint(params["max_items"]) + " items"
	case "type_mismatch":
		message = label + " must be " + fmt.Sprint(params["expected"]) + ", but got " + fmt.Sprint(params["received"])
	default:
		message = label + " does not satisfy " + strconv.Quote(rule)
	}
	if message == "" {
		return ToolArgumentIssue{}, false
	}
	return ToolArgumentIssue{Field: field, Rule: rule, Code: code, Message: message}, true
}

func patternValidationMessage(label, pattern string) string {
	switch {
	case pattern == `\S`:
		return label + " must not be blank"
	case strings.HasPrefix(pattern, "^") && isLiteralPattern(strings.TrimPrefix(pattern, "^")):
		return label + " must start with " + strconv.Quote(strings.TrimPrefix(pattern, "^"))
	case strings.HasSuffix(pattern, "$") && isLiteralPattern(strings.TrimSuffix(pattern, "$")):
		return label + " must end with " + strconv.Quote(strings.TrimSuffix(pattern, "$"))
	case isLiteralPattern(pattern):
		return label + " must contain " + strconv.Quote(pattern)
	default:
		return label + " must match pattern " + strconv.Quote(pattern)
	}
}

func isLiteralPattern(pattern string) bool {
	return pattern != "" && !strings.ContainsAny(pattern, `\\[](){}.*+?|^$`)
}

func joinField(parent, child string) string {
	if parent == "" {
		return child
	}
	if child == "" {
		return parent
	}
	return parent + "." + child
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

func joinFieldPath(prefix, field string) string {
	if prefix == "" {
		return field
	}
	return prefix + "." + field
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
