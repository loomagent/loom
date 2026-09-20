package loom

import (
	"context"
	"fmt"
)

// Arg is one declaration in an args contract: either a typed argument or a
// whole-call validator. The interface is sealed — the set of accepted argument
// shapes is closed, so a contract cannot silently accept something the runtime
// has no way to read back.
type Arg interface {
	declare(*argsBuilder) error
}

// argSpec is the flattened description of one argument, shared by every typed
// builder and consumed once when the contract is built. Constraints that a
// given kind cannot use are simply left nil and never projected.
type argSpec struct {
	name        string
	kind        argKind
	description string
	required    bool
	enum        []any
	format      string
	pattern     string
	minLength   *int
	maxLength   *int
	minimum     *float64
	maximum     *float64
	minItems    *int
	maxItems    *int
	uniqueItems bool
	field       func(ctx context.Context, value any) error
}

// argsBuilder accumulates declarations in order. Property order is preserved
// so the model-facing schema lists arguments the way the author wrote them.
type argsBuilder struct {
	order  []*argSpec
	byName map[string]*argSpec
	whole  []func(ctx context.Context, args Args) error
}

func newArgsBuilder() *argsBuilder {
	return &argsBuilder{byName: make(map[string]*argSpec)}
}

func (b *argsBuilder) add(spec *argSpec) error {
	if spec.name == "" {
		return fmt.Errorf("argument name is empty")
	}
	if _, exists := b.byName[spec.name]; exists {
		return fmt.Errorf("argument %q is declared more than once", spec.name)
	}
	b.byName[spec.name] = spec
	b.order = append(b.order, spec)
	return nil
}

// argValidator is the whole-call validator declared with ValidateArgs. It is an
// Arg so that a contract reads as one flat declaration list.
type argValidator struct {
	fn func(ctx context.Context, args Args) error
}

func (v argValidator) declare(b *argsBuilder) error {
	b.whole = append(b.whole, v.fn)
	return nil
}

// formatPatterns holds a shape check for formats whose grammar is simple enough
// to express as a regular expression. When a format has an entry and the author
// did not set an explicit pattern, the contract projects both format and
// pattern: format is what spec-compliant providers honour, pattern is what the
// rest fall back to, and local validation enforces both. Authors set only the
// format and get both.
var formatPatterns = map[string]string{
	"date":      `^\d{4}-\d{2}-\d{2}$`,
	"time":      `^\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})?$`,
	"date-time": `^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|z|[+-]\d{2}:\d{2})$`,
	"uuid":      `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
}

// String declares a string argument. String returns the builder for chaining.
func String(name string) *StringArg {
	return &StringArg{spec: &argSpec{name: name, kind: argKindString}}
}

// Enum declares a string argument restricted to values. The value is required
// to be one of the listed strings when present.
func Enum(name string, values ...string) *StringArg {
	arg := String(name)
	for _, value := range values {
		arg.spec.enum = append(arg.spec.enum, value)
	}
	return arg
}

// Date declares a string argument holding an ISO 8601 calendar date
// (YYYY-MM-DD). It is shorthand for Format("date").
func Date(name string) *StringArg { return String(name).Format("date") }

// Time declares a string argument holding an ISO 8601 time of day. It is
// shorthand for Format("time").
func Time(name string) *StringArg { return String(name).Format("time") }

// DateTime declares a string argument holding an RFC 3339 timestamp. It is
// shorthand for Format("date-time").
func DateTime(name string) *StringArg { return String(name).Format("date-time") }

// StringArg declares and configures a string argument.
type StringArg struct{ spec *argSpec }

func (a *StringArg) declare(b *argsBuilder) error { return b.add(a.spec) }

// Required marks the argument as mandatory.
func (a *StringArg) Required() *StringArg {
	a.spec.required = true
	return a
}

// Desc sets the model-facing argument description.
func (a *StringArg) Desc(text string) *StringArg {
	a.spec.description = text
	return a
}

// MinLen sets the minimum string length.
func (a *StringArg) MinLen(n int) *StringArg {
	a.spec.minLength = &n
	return a
}

// MaxLen sets the maximum string length.
func (a *StringArg) MaxLen(n int) *StringArg {
	a.spec.maxLength = &n
	return a
}

// Pattern sets an explicit ECMA 262 regular expression the value must match.
// An explicit pattern takes precedence over the one implied by Format.
func (a *StringArg) Pattern(pattern string) *StringArg {
	a.spec.pattern = pattern
	return a
}

// Format sets the JSON Schema format. Formats with a known regular expression
// also project a pattern; see formatPatterns.
func (a *StringArg) Format(format string) *StringArg {
	a.spec.format = format
	return a
}

// Validate registers a per-field check that runs after schema validation, only
// when the model actually sent the argument. Return Invalid or InvalidAt to
// report a model-facing problem; any other error is treated as an internal
// failure. A single validator may report several problems with errors.Join.
func (a *StringArg) Validate(fn func(ctx context.Context, value string) error) *StringArg {
	a.spec.field = func(ctx context.Context, value any) error {
		text, _ := value.(string)
		return fn(ctx, text)
	}
	return a
}

// Int declares an integer argument.
func Int(name string) *IntArg {
	return &IntArg{spec: &argSpec{name: name, kind: argKindInt}}
}

// IntArg declares and configures an integer argument.
type IntArg struct{ spec *argSpec }

func (a *IntArg) declare(b *argsBuilder) error { return b.add(a.spec) }

func (a *IntArg) Required() *IntArg { a.spec.required = true; return a }
func (a *IntArg) Desc(text string) *IntArg {
	a.spec.description = text
	return a
}

// Min sets the inclusive lower bound.
func (a *IntArg) Min(n int64) *IntArg {
	value := float64(n)
	a.spec.minimum = &value
	return a
}

// Max sets the inclusive upper bound.
func (a *IntArg) Max(n int64) *IntArg {
	value := float64(n)
	a.spec.maximum = &value
	return a
}

// Validate registers a per-field check; see StringArg.Validate.
func (a *IntArg) Validate(fn func(ctx context.Context, value int64) error) *IntArg {
	a.spec.field = func(ctx context.Context, value any) error {
		number, _ := value.(float64)
		return fn(ctx, int64(number))
	}
	return a
}

// Float declares a number argument.
func Float(name string) *FloatArg {
	return &FloatArg{spec: &argSpec{name: name, kind: argKindFloat}}
}

// FloatArg declares and configures a number argument.
type FloatArg struct{ spec *argSpec }

func (a *FloatArg) declare(b *argsBuilder) error { return b.add(a.spec) }

func (a *FloatArg) Required() *FloatArg { a.spec.required = true; return a }
func (a *FloatArg) Desc(text string) *FloatArg {
	a.spec.description = text
	return a
}
func (a *FloatArg) Min(n float64) *FloatArg { a.spec.minimum = &n; return a }
func (a *FloatArg) Max(n float64) *FloatArg { a.spec.maximum = &n; return a }

// Validate registers a per-field check; see StringArg.Validate.
func (a *FloatArg) Validate(fn func(ctx context.Context, value float64) error) *FloatArg {
	a.spec.field = func(ctx context.Context, value any) error {
		number, _ := value.(float64)
		return fn(ctx, number)
	}
	return a
}

// Bool declares a boolean argument.
func Bool(name string) *BoolArg {
	return &BoolArg{spec: &argSpec{name: name, kind: argKindBool}}
}

// BoolArg declares and configures a boolean argument.
type BoolArg struct{ spec *argSpec }

func (a *BoolArg) declare(b *argsBuilder) error { return b.add(a.spec) }

func (a *BoolArg) Required() *BoolArg { a.spec.required = true; return a }
func (a *BoolArg) Desc(text string) *BoolArg {
	a.spec.description = text
	return a
}

// Validate registers a per-field check; see StringArg.Validate.
func (a *BoolArg) Validate(fn func(ctx context.Context, value bool) error) *BoolArg {
	a.spec.field = func(ctx context.Context, value any) error {
		flag, _ := value.(bool)
		return fn(ctx, flag)
	}
	return a
}

// Strings declares an array-of-strings argument.
func Strings(name string) *StringsArg {
	return &StringsArg{spec: &argSpec{name: name, kind: argKindStrings}}
}

// StringsArg declares and configures an array-of-strings argument.
type StringsArg struct{ spec *argSpec }

func (a *StringsArg) declare(b *argsBuilder) error { return b.add(a.spec) }

func (a *StringsArg) Required() *StringsArg { a.spec.required = true; return a }
func (a *StringsArg) Desc(text string) *StringsArg {
	a.spec.description = text
	return a
}

// MinItems sets the minimum number of elements.
func (a *StringsArg) MinItems(n int) *StringsArg { a.spec.minItems = &n; return a }

// MaxItems sets the maximum number of elements.
func (a *StringsArg) MaxItems(n int) *StringsArg { a.spec.maxItems = &n; return a }

// Unique requires the elements to be distinct.
func (a *StringsArg) Unique() *StringsArg { a.spec.uniqueItems = true; return a }

// Validate registers a per-field check; see StringArg.Validate.
func (a *StringsArg) Validate(fn func(ctx context.Context, value []string) error) *StringsArg {
	a.spec.field = func(ctx context.Context, value any) error {
		items, _ := value.([]any)
		out := make([]string, 0, len(items))
		for _, item := range items {
			text, _ := item.(string)
			out = append(out, text)
		}
		return fn(ctx, out)
	}
	return a
}

// ValidateArgs registers a whole-call validator that runs after every field
// validator. Use it for rules that span arguments, for example a range whose
// lower bound must not exceed its upper bound. It follows the same error
// contract as field validators.
func ValidateArgs(fn func(ctx context.Context, args Args) error) Arg {
	return argValidator{fn: fn}
}
