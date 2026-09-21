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
	issues := make([]ToolArgumentIssue, 0, len(err.Violations))
	collectValidationIssues(err.Violations, &issues)
	sortIssues(issues)
	return issues
}

// collectValidationIssues renders each violation the validator emitted. The
// validator's codes are a closed set tied to the modelled keywords, so every
// violation becomes exactly one issue.
func collectValidationIssues(violations []toolcontract.Violation, issues *[]ToolArgumentIssue) {
	for _, violation := range violations {
		*issues = append(*issues, validationIssue(violation.Field(), violation))
	}
}

func validationIssue(field string, violation toolcontract.Violation) ToolArgumentIssue {
	label := quoteField(field)
	rule := violation.Keyword
	code := violation.Code
	params := violation.Params
	message := ""
	switch code {
	case "missing_required_property":
		property := strings.Trim(fmt.Sprint(params["property"]), "'")
		field = joinField(field, property)
		message = quoteField(field) + " is required"
	case "additional_property_mismatch":
		property := strings.Trim(fmt.Sprint(params["property"]), "'")
		field = joinField(field, property)
		rule = "unknown"
		message = quoteField(field) + " is not an accepted field"
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
	case "const_mismatch":
		message = label + " must equal " + compactJSON(params["expected"])
	case "unique_items_mismatch":
		message = label + " must contain unique items"
	case "pattern_mismatch":
		message = patternValidationMessage(label, fmt.Sprint(params["pattern"]))
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
		// The validator does not emit this code today. It still has to render: a
		// model that receives a blank line cannot fix its call.
		message = label + " does not satisfy " + strconv.Quote(rule)
	}
	return ToolArgumentIssue{Field: field, Rule: rule, Code: code, Message: message}
}

func patternValidationMessage(label, pattern string) string {
	switch {
	case formatOfProjectedPattern(pattern) != "":
		return label + " must be " + formatProse[formatOfProjectedPattern(pattern)]
	case pattern == notBlankPattern:
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

// isFormatProjection reports whether a pattern is only the shape check its format
// projects. The declared-argument builder adds that pattern itself, so naming the format
// says everything the pattern would, in terms a model can act on.
func isFormatProjection(schema *Schema) bool {
	if schema.Format == "" || schema.Pattern == "" {
		return false
	}
	return formatPatterns[schema.Format] == schema.Pattern
}

// formatOfProjectedPattern names the format a pattern is the shape check for. A violation
// carries only the pattern it broke, so this is what lets the message describe the shape
// the author asked for instead of the regular expression the contract derived. An author
// who writes the same pattern by hand gets the same answer.
func formatOfProjectedPattern(pattern string) string {
	for format, projected := range formatPatterns {
		if projected == pattern {
			return format
		}
	}
	return ""
}

// schemaPatterns lists the patterns a model has to satisfy, in the order the builder
// declared them: the property's own pattern, then one per allOf branch. A format's shape
// check is left out because "format date" already names it.
func schemaPatterns(schema *Schema) []string {
	var patterns []string
	if schema.Pattern != "" && !isFormatProjection(schema) {
		patterns = append(patterns, schema.Pattern)
	}
	for _, branch := range schema.AllOf {
		if branch.Pattern != "" {
			patterns = append(patterns, branch.Pattern)
		}
	}
	return patterns
}

// formatProse describes a format the way a model can act on it. It covers the formats
// whose pattern the validator enforces; a format without prose still gets its pattern.
var formatProse = map[string]string{
	"date":      "a date (YYYY-MM-DD)",
	"time":      "a time (HH:MM:SS, with an optional fractional part and timezone)",
	"date-time": "an RFC 3339 timestamp",
	"uuid":      "a UUID",
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

func summarizeExpectedArguments(schema *Schema) string {
	if schema == nil || !schemaHasType(schema, "object") {
		return ""
	}
	names := schema.PropertyNames()
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

func summarizeProperty(schema *Schema, required bool) string {
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

func appendSchemaConstraintParts(parts []string, schema *Schema) []string {
	if schema == nil {
		return parts
	}
	if schema.Const != nil {
		parts = append(parts, "equals "+compactJSON(schema.Const))
	}
	if schema.Enum != nil {
		parts = append(parts, "one of "+compactJSON(schema.Enum))
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
	if len(schemaPatterns(schema)) > 0 {
		for _, pattern := range schemaPatterns(schema) {
			switch {
			case pattern == notBlankPattern:
				parts = append(parts, "non-blank")
			default:
				if literal, kind, ok := literalPatternConstraint(pattern); ok {
					parts = append(parts, kind+" "+strconv.Quote(literal))
				} else {
					parts = append(parts, "pattern "+strconv.Quote(pattern))
				}
			}
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
	return parts
}

func schemaTypeName(schema *Schema) string {
	if schema == nil || schema.Type == "" {
		return "value"
	}
	return schema.Type
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

// literalPatternConstraint reports whether pattern is a plain literal in one of
// the three shapes a model reads well, so the summary can say "starts with"
// rather than show a regular expression.
func literalPatternConstraint(pattern string) (literal, kind string, ok bool) {
	kind = "contains"
	if strings.HasPrefix(pattern, "^") {
		kind = "starts with"
		pattern = strings.TrimPrefix(pattern, "^")
	}
	if strings.HasSuffix(pattern, "$") {
		if kind != "contains" {
			return "", "", false
		}
		kind = "ends with"
		pattern = strings.TrimSuffix(pattern, "$")
	}
	literal = unquoteRegexpLiteral(pattern)
	return literal, kind, regexp.QuoteMeta(literal) == pattern
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
