// Package toolcontract compiles and validates the JSON Schema subset used by
// Loom tool arguments. It isolates the schema engine from the stable,
// LLM-facing violation protocol consumed by the loom package.
//
// The engine is Loom's own: the schemas it validates are built by Loom's
// declared-argument builder, so the accepted keyword set is closed. A schema
// that uses a keyword outside that set is rejected at Compile time rather than
// silently ignored.
package toolcontract

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/loomagent/loom/internal/schema"
)

// Validator is an immutable compiled contract.
type Validator struct {
	schema *schema.Schema
}

// Violation is one machine-readable tool-contract failure.
type Violation struct {
	JSONPointer string
	Keyword     string
	Code        string
	Params      map[string]any
}

// ValidationError reports all schema violations found in one tool call.
type ValidationError struct {
	Violations []Violation
}

func (e *ValidationError) Error() string { return "tool arguments do not match the contract" }

// Compile checks that a schema stays inside the supported keyword set and
// returns a validator for it.
func Compile(s *schema.Schema) (*Validator, error) {
	if s == nil {
		return nil, fmt.Errorf("toolcontract: schema is nil")
	}
	if err := checkSupported(s, ""); err != nil {
		return nil, err
	}
	return &Validator{schema: s}, nil
}

// Validate checks one already syntax-validated JSON value.
func (v *Validator) Validate(raw jsontext.Value) *ValidationError {
	var violations []Violation
	validate(v.schema, decodeValue(raw), "", &violations)
	if len(violations) == 0 {
		return nil
	}
	return &ValidationError{Violations: violations}
}

// Field converts a violation's RFC 6901 JSON Pointer to Loom's dotted field
// notation. Escaped member names are decoded before joining.
func (v Violation) Field() string {
	pointer := strings.TrimPrefix(v.JSONPointer, "/")
	if pointer == "" {
		return ""
	}
	parts := strings.Split(pointer, "/")
	for index := range parts {
		parts[index] = strings.ReplaceAll(strings.ReplaceAll(parts[index], "~1", "/"), "~0", "~")
	}
	return strings.Join(parts, ".")
}

// jsonNumber keeps a JSON number's exact text. Decoding into float64 would round
// integers beyond 2**53, and a const such as 9007199254740993 must not match
// 9007199254740992.
type jsonNumber string

func (n jsonNumber) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// decodeValue decodes one JSON value, keeping numbers exact and objects and
// arrays as Go maps and slices.
func decodeValue(raw jsontext.Value) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	switch trimmed[0] {
	case '{':
		var object map[string]jsontext.Value
		if err := jsonv2.Unmarshal(raw, &object); err != nil {
			return nil
		}
		out := make(map[string]any, len(object))
		for name, value := range object {
			out[name] = decodeValue(value)
		}
		return out
	case '[':
		var items []jsontext.Value
		if err := jsonv2.Unmarshal(raw, &items); err != nil {
			return nil
		}
		out := make([]any, len(items))
		for index, item := range items {
			out[index] = decodeValue(item)
		}
		return out
	case '"':
		var text string
		if err := jsonv2.Unmarshal(raw, &text); err != nil {
			return nil
		}
		return text
	}
	switch string(trimmed) {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	return jsonNumber(string(trimmed))
}

// checkSupported rejects keywords the validator does not implement, so an
// unsupported constraint fails loudly instead of passing silently.
func checkSupported(s *schema.Schema, path string) error {
	if s == nil {
		return nil
	}
	at := path
	if at == "" {
		at = "root"
	}
	unsupported := func(keyword string) error {
		return fmt.Errorf("toolcontract: unsupported JSON Schema keyword %s at %s", keyword, at)
	}
	switch {
	case s.Ref != "":
		return unsupported("$ref")
	case s.Defs != nil:
		return unsupported("$defs")
	case s.Not != nil:
		return unsupported("not")
	case len(s.AllOf) > 0:
		return unsupported("allOf")
	case len(s.AnyOf) > 0:
		return unsupported("anyOf")
	case len(s.OneOf) > 0:
		return unsupported("oneOf")
	}
	if s.Type != "" && !knownType(s.Type) {
		return fmt.Errorf("toolcontract: unknown type %q at %s", s.Type, at)
	}
	if s.Pattern != "" {
		if _, err := regexp.Compile(s.Pattern); err != nil {
			return fmt.Errorf("toolcontract: invalid pattern at %s: %w", at, err)
		}
	}
	for _, name := range PropertyNames(s) {
		if err := checkSupported(s.Properties[name], path+"/properties/"+name); err != nil {
			return err
		}
	}
	return checkSupported(s.Items, path+"/items")
}

// PropertyNames returns a schema's property names in declaration order, then
// alphabetically for anything without a recorded order.
func PropertyNames(s *schema.Schema) []string {
	if s == nil {
		return nil
	}
	names := append([]string(nil), s.PropertyOrder...)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	var remaining []string
	for name := range s.Properties {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(names, remaining...)
}

func knownType(name string) bool {
	switch name {
	case "object", "array", "string", "boolean", "null", "number", "integer":
		return true
	default:
		return false
	}
}

func validate(s *schema.Schema, value any, pointer string, out *[]Violation) {
	if s == nil {
		return
	}
	if s.Const != nil && !equalJSON(value, *s.Const) {
		appendViolation(out, pointer, "const", "const_mismatch", map[string]any{"expected": *s.Const})
	}
	if len(s.Enum) > 0 && !inEnum(value, s.Enum) {
		appendViolation(out, pointer, "enum", "value_not_in_enum", map[string]any{"allowed": s.Enum})
	}
	if s.Type != "" && !matchesType(s.Type, value) {
		appendViolation(out, pointer, "type", "type_mismatch", map[string]any{
			"expected": s.Type,
			"received": jsonTypeName(value),
		})
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		validateObject(s, typed, pointer, out)
	case []any:
		validateArray(s, typed, pointer, out)
	case string:
		validateString(s, typed, pointer, out)
	case jsonNumber:
		validateNumber(s, typed, pointer, out)
	}
}

func validateObject(s *schema.Schema, object map[string]any, pointer string, out *[]Violation) {
	for _, name := range s.Required {
		if _, present := object[name]; !present {
			appendViolation(out, pointer, "required", "missing_required_property", map[string]any{"property": name})
		}
	}
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if property, ok := s.Properties[name]; ok {
			validate(property, object[name], joinPointer(pointer, name), out)
			continue
		}
		if s.AdditionalProperties != nil && !*s.AdditionalProperties {
			appendViolation(out, pointer, "additionalProperties", "additional_property_mismatch", map[string]any{"property": name})
		}
	}
}

func validateArray(s *schema.Schema, items []any, pointer string, out *[]Violation) {
	if s.MinItems != nil && len(items) < *s.MinItems {
		appendViolation(out, pointer, "minItems", "items_too_short", map[string]any{"min_items": *s.MinItems})
	}
	if s.MaxItems != nil && len(items) > *s.MaxItems {
		appendViolation(out, pointer, "maxItems", "items_too_long", map[string]any{"max_items": *s.MaxItems})
	}
	if s.UniqueItems {
		seen := make(map[string]bool, len(items))
		for _, item := range items {
			key := canonicalJSON(item)
			if seen[key] {
				appendViolation(out, pointer, "uniqueItems", "unique_items_mismatch", nil)
				break
			}
			seen[key] = true
		}
	}
	if s.Items != nil {
		for index, item := range items {
			validate(s.Items, item, fmt.Sprintf("%s/%d", pointer, index), out)
		}
	}
}

func validateString(s *schema.Schema, value, pointer string, out *[]Violation) {
	if s.MinLength != nil {
		if length := utf8.RuneCountInString(value); length < *s.MinLength {
			appendViolation(out, pointer, "minLength", "string_too_short", map[string]any{"min_length": *s.MinLength})
		}
	}
	if s.MaxLength != nil {
		if length := utf8.RuneCountInString(value); length > *s.MaxLength {
			appendViolation(out, pointer, "maxLength", "string_too_long", map[string]any{"max_length": *s.MaxLength})
		}
	}
	if s.Pattern != "" {
		pattern, err := regexp.Compile(s.Pattern)
		if err == nil && !pattern.MatchString(value) {
			appendViolation(out, pointer, "pattern", "pattern_mismatch", map[string]any{"pattern": s.Pattern})
		}
	}
}

func validateNumber(s *schema.Schema, value jsonNumber, pointer string, out *[]Violation) {
	number, err := strconv.ParseFloat(string(value), 64)
	if err != nil {
		return
	}
	if s.Minimum != nil && number < *s.Minimum {
		appendViolation(out, pointer, "minimum", "value_below_minimum", map[string]any{"minimum": *s.Minimum})
	}
	if s.Maximum != nil && number > *s.Maximum {
		appendViolation(out, pointer, "maximum", "value_above_maximum", map[string]any{"maximum": *s.Maximum})
	}
	if s.ExclusiveMinimum != nil && number <= *s.ExclusiveMinimum {
		appendViolation(out, pointer, "exclusiveMinimum", "exclusive_minimum_mismatch", map[string]any{"exclusive_minimum": *s.ExclusiveMinimum})
	}
	if s.ExclusiveMaximum != nil && number >= *s.ExclusiveMaximum {
		appendViolation(out, pointer, "exclusiveMaximum", "exclusive_maximum_mismatch", map[string]any{"exclusive_maximum": *s.ExclusiveMaximum})
	}
}

func appendViolation(out *[]Violation, pointer, keyword, code string, params map[string]any) {
	*out = append(*out, Violation{JSONPointer: pointer, Keyword: keyword, Code: code, Params: params})
}

func joinPointer(pointer, name string) string {
	name = strings.ReplaceAll(name, "~", "~0")
	name = strings.ReplaceAll(name, "/", "~1")
	return pointer + "/" + name
}

func matchesType(want string, value any) bool {
	switch want {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "number":
		_, ok := value.(jsonNumber)
		return ok
	case "integer":
		number, ok := value.(jsonNumber)
		// A JSON number is an integer when it has neither a fraction nor an
		// exponent, which is exact regardless of magnitude.
		return ok && !strings.ContainsAny(string(number), ".eE")
	default:
		return false
	}
}

func jsonTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case jsonNumber:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "value"
	}
}

func inEnum(value any, allowed []any) bool {
	for _, candidate := range allowed {
		if equalJSON(value, candidate) {
			return true
		}
	}
	return false
}

func equalJSON(left, right any) bool {
	if leftNumber, ok := numberRat(left); ok {
		rightNumber, ok := numberRat(right)
		return ok && leftNumber.Cmp(rightNumber) == 0
	}
	if _, ok := numberRat(right); ok {
		return false
	}
	return canonicalJSON(left) == canonicalJSON(right)
}

func numberRat(value any) (*big.Rat, bool) {
	switch typed := value.(type) {
	case jsonNumber:
		number, ok := new(big.Rat).SetString(string(typed))
		return number, ok
	case float64:
		if math.IsInf(typed, 0) || math.IsNaN(typed) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(typed), true
	default:
		return nil, false
	}
}

func canonicalJSON(value any) string {
	data, err := jsonv2.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}
