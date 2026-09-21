package loom

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
)

// Declaration is one entry in an args contract: a typed argument or a whole-call
// validator. The interface is sealed — the set of accepted shapes is closed, so
// a contract cannot silently accept something the runtime has no way to read
// back.
type Declaration interface {
	declare(*argsBuilder) error
}

// Handle names one declared argument. It is sealed: only this package can
// implement it, so a whole-call rule can only refer to real arguments.
type Handle interface {
	argumentName() string
}

// HandleWith is a typed handle. Concrete handles implement it through the
// embedded Arg[T], which is what lets a whole-call validator take typed
// parameters instead of reading string keys out of Args.
type HandleWith[T any] interface {
	Handle
	Get(args Args) T
}

// Arg is the typed base of every argument handle. Get reads the argument from
// the validated Args without a string key or a type assertion, because the type
// was fixed where the argument was declared.
type Arg[T any] struct {
	spec *argSpec
}

// Get returns the argument value, or the zero value when the model omitted it.
func (a *Arg[T]) Get(args Args) T {
	var out T
	raw, ok := args.values[a.spec.name]
	if !ok {
		// A miss is either an optional argument the model omitted or a handle this
		// contract never declared. Only the second is a mistake, and only on this path
		// does it cost anything to tell them apart.
		args.requireDeclared(a.spec.name)
		return out
	}
	if err := jsonv2.Unmarshal(raw, &out); err != nil {
		// Decode checked every present argument against its Go type, so a
		// failure here means the contract and this handle disagree.
		panic(fmt.Sprintf("loom: read tool argument %q: %v", a.spec.name, err))
	}
	return out
}

// Present reports whether the model sent the argument, which distinguishes an
// omitted optional argument from one sent as its zero value.
func (a *Arg[T]) Present(args Args) bool {
	_, ok := args.values[a.spec.name]
	if !ok {
		args.requireDeclared(a.spec.name)
	}
	return ok
}

func (a *Arg[T]) argumentName() string { return a.spec.name }

// FieldValidator checks one argument after schema validation. Prefer a named
// function over a literal for anything non-trivial, so the rule can be unit
// tested directly and is identifiable in stack traces.
type FieldValidator[T any] func(ctx context.Context, value T) error

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
	notBlank    bool
	minLength   *int
	maxLength   *int
	minimum     *float64
	maximum     *float64
	// Strict bounds. Like Min/Max they are projected into the schema the model
	// receives and enforced on the model's arguments locally.
	exclusiveMinimum *float64
	exclusiveMaximum *float64
	minItems         *int
	maxItems         *int
	uniqueItems      bool
	examples         []any
	validators       []func(ctx context.Context, value jsontext.Value) error
}

// argsBuilder accumulates declarations in order. Property order is preserved
// so the model-facing schema lists arguments the way the author wrote them.
type argsBuilder struct {
	order  []*argSpec
	byName map[string]*argSpec
	whole  []wholeValidator
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

// wholeValidator is one declared whole-call check together with the arguments
// it reads. Cross and friends build these from typed handles.
type wholeValidator struct {
	fields []string
	fn     func(ctx context.Context, args Args) error
}

func (v wholeValidator) declare(b *argsBuilder) error {
	// The declared fields are the rule's dependencies, so a typo is a contract
	// error rather than a rule that silently never reads the argument it was
	// written for.
	for _, name := range v.fields {
		if _, ok := b.byName[name]; !ok {
			return fmt.Errorf("whole-call rule depends on undeclared argument %q", name)
		}
	}
	b.whole = append(b.whole, v)
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

// notBlankPattern is the standard JSON Schema spelling of "not empty or whitespace":
// a pattern, which the specification searches rather than anchors, so it asks for one
// non-whitespace character anywhere in the value.
const notBlankPattern = `\S`

// String declares a string argument.
func String(name string) *StringArg {
	return &StringArg{Arg[string]{spec: &argSpec{name: name, kind: argKindString}}}
}

// Enum declares a string argument restricted to values.
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

// UUID declares a string argument holding a UUID. It is shorthand for
// Format("uuid").
func UUID(name string) *StringArg { return String(name).Format("uuid") }

// StringArg declares and configures a string argument.
type StringArg struct{ Arg[string] }

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

// Example attaches a model-facing example value. When every required argument
// has one, the assembled example call is offered to the model for reference.
func (a *StringArg) Example(value string) *StringArg {
	a.spec.examples = append(a.spec.examples, value)
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

// NotBlank rejects values that are empty or contain only whitespace. It is the standard
// JSON Schema spelling of that rule, so the model and the provider both see it before the
// call instead of learning it from a failure. It says nothing about whether the model
// must send the argument: an omitted optional argument still passes, exactly as a
// property-level keyword does. Use Required for presence.
func (a *StringArg) NotBlank() *StringArg {
	a.spec.notBlank = true
	return a
}

// Validate registers a per-field check that runs after schema validation, only
// when the model actually sent the argument. Return Invalid or InvalidOn to
// report a model-facing problem; any other error is treated as an internal
// failure. A single validator may report several problems with errors.Join.
func (a *StringArg) Validate(fn FieldValidator[string]) *StringArg {
	a.spec.validators = append(a.spec.validators, func(ctx context.Context, raw jsontext.Value) error {
		var value string
		if err := jsonv2.Unmarshal(raw, &value); err != nil {
			return err
		}
		return fn(ctx, value)
	})
	return a
}

// Uint declares a non-negative integer argument. Unsigned is deliberate: tool
// arguments that count things cannot be negative, and rejecting negatives in
// the schema keeps that invariant in the Go type as well.
func Uint(name string) *UintArg {
	return &UintArg{Arg[uint64]{spec: &argSpec{name: name, kind: argKindUint}}}
}

// UintArg declares and configures a non-negative integer argument.
type UintArg struct{ Arg[uint64] }

func (a *UintArg) declare(b *argsBuilder) error { return b.add(a.spec) }

func (a *UintArg) Required() *UintArg { a.spec.required = true; return a }
func (a *UintArg) Desc(text string) *UintArg {
	a.spec.description = text
	return a
}

// Example attaches a model-facing example value; see StringArg.Example.
func (a *UintArg) Example(value uint64) *UintArg {
	a.spec.examples = append(a.spec.examples, value)
	return a
}

// Min sets the inclusive lower bound.
func (a *UintArg) Min(n uint64) *UintArg {
	value := float64(n)
	a.spec.minimum = &value
	return a
}

// Max sets the inclusive upper bound.
func (a *UintArg) Max(n uint64) *UintArg {
	value := float64(n)
	a.spec.maximum = &value
	return a
}

// ExclusiveMin requires the value to be strictly greater than n. It is
// projected into the schema and enforced locally, so the model sees the bound
// and the check applies to whatever it sends.
func (a *UintArg) ExclusiveMin(n uint64) *UintArg {
	value := float64(n)
	a.spec.exclusiveMinimum = &value
	return a
}

// ExclusiveMax requires the value to be strictly less than n; see ExclusiveMin.
func (a *UintArg) ExclusiveMax(n uint64) *UintArg {
	value := float64(n)
	a.spec.exclusiveMaximum = &value
	return a
}

// Validate registers a per-field check; see StringArg.Validate.
func (a *UintArg) Validate(fn FieldValidator[uint64]) *UintArg {
	a.spec.validators = append(a.spec.validators, func(ctx context.Context, raw jsontext.Value) error {
		var value uint64
		if err := jsonv2.Unmarshal(raw, &value); err != nil {
			return err
		}
		return fn(ctx, value)
	})
	return a
}

// Float declares a number argument.
func Float(name string) *FloatArg {
	return &FloatArg{Arg[float64]{spec: &argSpec{name: name, kind: argKindFloat}}}
}

// FloatArg declares and configures a number argument.
type FloatArg struct{ Arg[float64] }

func (a *FloatArg) declare(b *argsBuilder) error { return b.add(a.spec) }

func (a *FloatArg) Required() *FloatArg { a.spec.required = true; return a }
func (a *FloatArg) Desc(text string) *FloatArg {
	a.spec.description = text
	return a
}

// Example attaches a model-facing example value; see StringArg.Example.
func (a *FloatArg) Example(value float64) *FloatArg {
	a.spec.examples = append(a.spec.examples, value)
	return a
}
func (a *FloatArg) Min(n float64) *FloatArg { a.spec.minimum = &n; return a }
func (a *FloatArg) Max(n float64) *FloatArg { a.spec.maximum = &n; return a }

// ExclusiveMin requires the value to be strictly greater than n; see
// UintArg.ExclusiveMin.
func (a *FloatArg) ExclusiveMin(n float64) *FloatArg { a.spec.exclusiveMinimum = &n; return a }

// ExclusiveMax requires the value to be strictly less than n; see
// UintArg.ExclusiveMin.
func (a *FloatArg) ExclusiveMax(n float64) *FloatArg { a.spec.exclusiveMaximum = &n; return a }

// Validate registers a per-field check; see StringArg.Validate.
func (a *FloatArg) Validate(fn FieldValidator[float64]) *FloatArg {
	a.spec.validators = append(a.spec.validators, func(ctx context.Context, raw jsontext.Value) error {
		var value float64
		if err := jsonv2.Unmarshal(raw, &value); err != nil {
			return err
		}
		return fn(ctx, value)
	})
	return a
}

// Bool declares a boolean argument.
func Bool(name string) *BoolArg {
	return &BoolArg{Arg[bool]{spec: &argSpec{name: name, kind: argKindBool}}}
}

// BoolArg declares and configures a boolean argument.
type BoolArg struct{ Arg[bool] }

func (a *BoolArg) declare(b *argsBuilder) error { return b.add(a.spec) }

func (a *BoolArg) Required() *BoolArg { a.spec.required = true; return a }
func (a *BoolArg) Desc(text string) *BoolArg {
	a.spec.description = text
	return a
}

// Example attaches a model-facing example value; see StringArg.Example.
func (a *BoolArg) Example(value bool) *BoolArg {
	a.spec.examples = append(a.spec.examples, value)
	return a
}

// Validate registers a per-field check; see StringArg.Validate.
func (a *BoolArg) Validate(fn FieldValidator[bool]) *BoolArg {
	a.spec.validators = append(a.spec.validators, func(ctx context.Context, raw jsontext.Value) error {
		var value bool
		if err := jsonv2.Unmarshal(raw, &value); err != nil {
			return err
		}
		return fn(ctx, value)
	})
	return a
}

// Strings declares an array-of-strings argument.
func Strings(name string) *StringsArg {
	return &StringsArg{Arg[[]string]{spec: &argSpec{name: name, kind: argKindStrings}}}
}

// StringsArg declares and configures an array-of-strings argument.
type StringsArg struct{ Arg[[]string] }

func (a *StringsArg) declare(b *argsBuilder) error { return b.add(a.spec) }

func (a *StringsArg) Required() *StringsArg { a.spec.required = true; return a }
func (a *StringsArg) Desc(text string) *StringsArg {
	a.spec.description = text
	return a
}

// Example attaches a model-facing example value; see StringArg.Example.
func (a *StringsArg) Example(values ...string) *StringsArg {
	copied := make([]string, len(values))
	copy(copied, values)
	a.spec.examples = append(a.spec.examples, copied)
	return a
}

// MinItems sets the minimum number of elements.
func (a *StringsArg) MinItems(n int) *StringsArg { a.spec.minItems = &n; return a }

// MaxItems sets the maximum number of elements.
func (a *StringsArg) MaxItems(n int) *StringsArg { a.spec.maxItems = &n; return a }

// Unique requires the elements to be distinct.
func (a *StringsArg) Unique() *StringsArg { a.spec.uniqueItems = true; return a }

// Validate registers a per-field check; see StringArg.Validate.
func (a *StringsArg) Validate(fn FieldValidator[[]string]) *StringsArg {
	a.spec.validators = append(a.spec.validators, func(ctx context.Context, raw jsontext.Value) error {
		var value []string
		if err := jsonv2.Unmarshal(raw, &value); err != nil {
			return err
		}
		return fn(ctx, value)
	})
	return a
}

// Cross declares a whole-call rule over two typed arguments. The handles name
// the arguments the rule reads, so every name is checked against the contract at
// build time and the rule is skipped when none of its arguments are present.
//
//	loom.Cross(dateFrom, dateTo).Using(validateDateRange)
//
// Prefer a named function over an inline literal, so the rule can be unit tested
// directly and is identifiable in stack traces.
func Cross[A, B any](first HandleWith[A], second HandleWith[B]) *CrossBuilder[A, B] {
	return &CrossBuilder[A, B]{first: first, second: second}
}

// CrossBuilder binds the arguments of a two-argument whole-call rule.
type CrossBuilder[A, B any] struct {
	first  HandleWith[A]
	second HandleWith[B]
}

// Using attaches the check. It is the only way to turn a builder into a
// Declaration, so a Cross that forgot its function does not compile.
func (c *CrossBuilder[A, B]) Using(fn func(ctx context.Context, first A, second B) error) Declaration {
	return wholeValidator{
		fields: []string{c.first.argumentName(), c.second.argumentName()},
		fn: func(ctx context.Context, args Args) error {
			return fn(ctx, c.first.Get(args), c.second.Get(args))
		},
	}
}
