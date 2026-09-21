// Package toolcontract compiles and validates the JSON Schema subset used by
// Loom tool arguments. It isolates the schema engine from the stable,
// LLM-facing violation protocol consumed by the loom package.
//
// The engine is Loom's own: the schemas it validates are built by Loom's
// declared-argument builder, so the accepted keyword set is closed. The model
// refuses to decode a keyword outside that set, so an unsupported constraint
// cannot be silently ignored.
package toolcontract

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
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
	// patterns caches every compiled "pattern" in the schema, so validation
	// never compiles a regular expression on a request path.
	patterns map[*schema.Schema]*regexp.Regexp
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
	patterns := make(map[*schema.Schema]*regexp.Regexp)
	if err := checkSupported(s, "", patterns); err != nil {
		return nil, err
	}
	return &Validator{schema: s, patterns: patterns}, nil
}

// Validate checks one already syntax-validated JSON value.
func (v *Validator) Validate(raw jsontext.Value) *ValidationError {
	run := &validation{patterns: v.patterns}
	run.visit(v.schema, decodeValue(raw), "")
	if len(run.violations) == 0 {
		return nil
	}
	return &ValidationError{Violations: run.violations}
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

// checkSupported validates that a schema stays inside the modelled subset and
// compiles every pattern once, caching it for later validation. Keywords outside
// the subset cannot reach here: the model refuses to decode them.
func checkSupported(s *schema.Schema, path string, patterns map[*schema.Schema]*regexp.Regexp) error {
	if s == nil {
		return nil
	}
	at := path
	if at == "" {
		at = "root"
	}
	if s.Type != "" && !knownType(s.Type) {
		return fmt.Errorf("toolcontract: unknown type %q at %s", s.Type, at)
	}
	if s.Format != "" && s.Pattern == "" {
		// The model would be told the format while nothing enforced it. A format
		// Loom can pattern, or an explicit pattern, keeps the advertised and the
		// enforced contract the same.
		return fmt.Errorf("toolcontract: format %q at %s has no pattern, so it would be advertised but not enforced; add a pattern or use a format Loom patterns", s.Format, at)
	}
	if s.Pattern != "" {
		compiled, err := regexp.Compile(s.Pattern)
		if err != nil {
			return fmt.Errorf("toolcontract: invalid pattern at %s: %w", at, err)
		}
		patterns[s] = compiled
	}
	for index, alternate := range s.AllOf {
		if err := checkSupported(alternate, fmt.Sprintf("%s/allOf/%d", path, index), patterns); err != nil {
			return err
		}
	}
	for _, name := range s.PropertyNames() {
		if err := checkSupported(s.Properties[name], path+"/properties/"+name, patterns); err != nil {
			return err
		}
	}
	return checkSupported(s.Items, path+"/items", patterns)
}

func knownType(name string) bool {
	switch name {
	case "object", "array", "string", "boolean", "null", "number", "integer":
		return true
	default:
		return false
	}
}

// validation accumulates the violations found in one value.
type validation struct {
	patterns   map[*schema.Schema]*regexp.Regexp
	violations []Violation
}

func (run *validation) add(pointer, keyword, code string, params map[string]any) {
	run.violations = append(run.violations, Violation{JSONPointer: pointer, Keyword: keyword, Code: code, Params: params})
}

func (run *validation) visit(s *schema.Schema, value any, pointer string) {
	if s == nil {
		return
	}
	if s.Const != nil {
		expected := decodeValue(s.Const)
		if !equalJSON(value, expected) {
			run.add(pointer, "const", "const_mismatch", map[string]any{"expected": expected})
		}
	}
	if s.Enum != nil && !inEnum(value, s.Enum) {
		run.add(pointer, "enum", "value_not_in_enum", map[string]any{"allowed": s.Enum})
	}
	if s.Type != "" && !matchesType(s.Type, value) {
		run.add(pointer, "type", "type_mismatch", map[string]any{
			"expected": s.Type,
			"received": jsonTypeName(value),
		})
		return
	}
	// Every branch constrains the same instance, so a value that breaks two of them
	// reports both and the caller can say so in one message.
	for _, alternate := range s.AllOf {
		run.visit(alternate, value, pointer)
	}
	switch typed := value.(type) {
	case map[string]any:
		run.object(s, typed, pointer)
	case []any:
		run.array(s, typed, pointer)
	case string:
		run.string(s, typed, pointer)
	case jsonNumber:
		run.number(s, typed, pointer)
	}
}

func (run *validation) object(s *schema.Schema, object map[string]any, pointer string) {
	for _, name := range s.Required {
		if _, present := object[name]; !present {
			run.add(pointer, "required", "missing_required_property", map[string]any{"property": name})
		}
	}
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if property, ok := s.Properties[name]; ok {
			run.visit(property, object[name], joinPointer(pointer, name))
			continue
		}
		if s.AdditionalProperties != nil && !*s.AdditionalProperties {
			run.add(pointer, "additionalProperties", "additional_property_mismatch", map[string]any{"property": name})
		}
	}
}

func (run *validation) array(s *schema.Schema, items []any, pointer string) {
	if s.MinItems != nil && len(items) < *s.MinItems {
		run.add(pointer, "minItems", "items_too_short", map[string]any{"min_items": *s.MinItems})
	}
	if s.MaxItems != nil && len(items) > *s.MaxItems {
		run.add(pointer, "maxItems", "items_too_long", map[string]any{"max_items": *s.MaxItems})
	}
	if s.UniqueItems {
		seen := make(map[string]bool, len(items))
		for _, item := range items {
			key := canonicalJSON(item)
			if seen[key] {
				run.add(pointer, "uniqueItems", "unique_items_mismatch", nil)
				break
			}
			seen[key] = true
		}
	}
	if s.Items != nil {
		for index, item := range items {
			run.visit(s.Items, item, fmt.Sprintf("%s/%d", pointer, index))
		}
	}
}

func (run *validation) string(s *schema.Schema, value, pointer string) {
	if s.MinLength != nil {
		if length := utf8.RuneCountInString(value); length < *s.MinLength {
			run.add(pointer, "minLength", "string_too_short", map[string]any{"min_length": *s.MinLength})
		}
	}
	if s.MaxLength != nil {
		if length := utf8.RuneCountInString(value); length > *s.MaxLength {
			run.add(pointer, "maxLength", "string_too_long", map[string]any{"max_length": *s.MaxLength})
		}
	}
	if s.Pattern != "" {
		if pattern := run.patterns[s]; pattern != nil && !pattern.MatchString(value) {
			run.add(pointer, "pattern", "pattern_mismatch", map[string]any{"pattern": s.Pattern})
		}
	}
}

func (run *validation) number(s *schema.Schema, value jsonNumber, pointer string) {
	number, err := strconv.ParseFloat(string(value), 64)
	if err != nil {
		return
	}
	if s.Minimum != nil && number < *s.Minimum {
		run.add(pointer, "minimum", "value_below_minimum", map[string]any{"minimum": *s.Minimum})
	}
	if s.Maximum != nil && number > *s.Maximum {
		run.add(pointer, "maximum", "value_above_maximum", map[string]any{"maximum": *s.Maximum})
	}
	if s.ExclusiveMinimum != nil && number <= *s.ExclusiveMinimum {
		run.add(pointer, "exclusiveMinimum", "exclusive_minimum_mismatch", map[string]any{"exclusive_minimum": *s.ExclusiveMinimum})
	}
	if s.ExclusiveMaximum != nil && number >= *s.ExclusiveMaximum {
		run.add(pointer, "exclusiveMaximum", "exclusive_maximum_mismatch", map[string]any{"exclusive_maximum": *s.ExclusiveMaximum})
	}
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
	return canonicalJSON(left) == canonicalJSON(right)
}

// canonicalJSON renders a decoded JSON value in a canonical form: object keys are
// sorted, numbers are reduced to an exact rational, and arrays keep their order.
// Marshaling and comparing text would not do: encoding/json/v2 does not order
// map keys, so the same object would compare unequal at random, and 1 would
// differ from 1.0.
func canonicalJSON(value any) string {
	var builder strings.Builder
	writeCanonical(&builder, value)
	return builder.String()
}

func writeCanonical(builder *strings.Builder, value any) {
	switch typed := value.(type) {
	case map[string]any:
		names := make([]string, 0, len(typed))
		for name := range typed {
			names = append(names, name)
		}
		sort.Strings(names)
		builder.WriteByte('{')
		for index, name := range names {
			if index > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(strconv.Quote(name))
			builder.WriteByte(':')
			writeCanonical(builder, typed[name])
		}
		builder.WriteByte('}')
	case []any:
		builder.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				builder.WriteByte(',')
			}
			writeCanonical(builder, item)
		}
		builder.WriteByte(']')
	case jsonNumber:
		writeCanonicalNumber(builder, string(typed))
	case float64:
		writeCanonicalNumber(builder, strconv.FormatFloat(typed, 'g', -1, 64))
	case string:
		builder.WriteString(strconv.Quote(typed))
	case bool:
		if typed {
			builder.WriteString("true")
		} else {
			builder.WriteString("false")
		}
	case nil:
		builder.WriteString("null")
	default:
		// decodeValue produces only the kinds above.
		builder.WriteString("null")
	}
}

func writeCanonicalNumber(builder *strings.Builder, text string) {
	if number, ok := new(big.Rat).SetString(text); ok {
		builder.WriteString(number.RatString())
		return
	}
	builder.WriteString(text)
}
