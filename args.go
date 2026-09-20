package loom

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
)

// Args is the validated view of one tool call's arguments, handed to handlers
// declared with NewArgsContract / NewArgsTool.
//
// Every getter is total: a declared optional argument the model omitted reads
// back as its zero value, so handlers do not repeat presence checks that the
// contract already performed. Asking for a name that was never declared, or for
// a declared name under the wrong type, is a programming error in the handler
// and panics — by the time a handler runs, the model's input has already been
// checked against the contract, so those can only fail for reasons the handler
// itself controls.
//
// Arguments are kept as raw JSON until a getter reads them, so numbers keep
// their full int64/uint64 precision instead of being rounded through float64.
type Args struct {
	values   map[string]jsontext.Value
	declared map[string]argKind
}

// Has reports whether the model sent a value for name. It is the only way to
// tell "omitted" from "present but empty" for optional arguments.
func (a Args) Has(name string) bool {
	a.declare(name)
	_, ok := a.values[name]
	return ok
}

// String returns a declared string argument, or "" when it was omitted.
func (a Args) String(name string) string {
	a.expect(name, argKindString)
	var value string
	a.decode(name, &value)
	return value
}

// Int returns a declared integer argument, or 0 when it was omitted.
func (a Args) Int(name string) int64 {
	a.expect(name, argKindInt)
	var value int64
	a.decode(name, &value)
	return value
}

// Float returns a declared number argument, or 0 when it was omitted.
func (a Args) Float(name string) float64 {
	a.expect(name, argKindFloat)
	var value float64
	a.decode(name, &value)
	return value
}

// Bool returns a declared boolean argument, or false when it was omitted.
func (a Args) Bool(name string) bool {
	a.expect(name, argKindBool)
	var value bool
	a.decode(name, &value)
	return value
}

// Strings returns a declared string-array argument, or nil when it was omitted.
// The returned slice is a copy; callers may mutate it freely.
func (a Args) Strings(name string) []string {
	a.expect(name, argKindStrings)
	var value []string
	a.decode(name, &value)
	return value
}

// Raw returns the argument decoded as a generic JSON value, or nil when it was
// omitted. Numbers are float64; use the typed getters when precision matters.
func (a Args) Raw(name string) any {
	a.declare(name)
	raw, ok := a.values[name]
	if !ok {
		return nil
	}
	var value any
	if err := jsonv2.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

func (a Args) decode(name string, out any) {
	raw, ok := a.values[name]
	if !ok {
		return
	}
	// The contract already validated this value against the schema, so a decode
	// failure here is a bug in the contract, not in the model's input.
	if err := jsonv2.Unmarshal(raw, out); err != nil {
		panic(fmt.Sprintf("loom: decode tool argument %q: %v", name, err))
	}
}

func (a Args) declare(name string) argKind {
	kind, ok := a.declared[name]
	if !ok {
		panic(fmt.Sprintf("loom: tool argument %q is not declared", name))
	}
	return kind
}

func (a Args) expect(name string, want argKind) {
	if got := a.declare(name); got != want {
		panic(fmt.Sprintf("loom: tool argument %q is %s, not %s", name, got, want))
	}
}

// argKind is the closed set of argument shapes the declared-argument API
// supports. It is deliberately small: each kind maps to exactly one JSON Schema
// type and one typed getter, so an argument can never be declared as one shape
// and read back as another.
type argKind uint8

const (
	argKindString argKind = iota
	argKindInt
	argKindFloat
	argKindBool
	argKindStrings
)

func (k argKind) String() string {
	switch k {
	case argKindString:
		return "string"
	case argKindInt:
		return "integer"
	case argKindFloat:
		return "number"
	case argKindBool:
		return "boolean"
	case argKindStrings:
		return "string array"
	default:
		return "unknown"
	}
}
