package loom

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
)

// Args is the validated view of one tool call's arguments. Handlers read it
// through the typed handles returned when the arguments were declared, so no
// string key appears at a read site:
//
//	args, err := contract.DecodeContext(ctx, argumentsJSON)
//	if err != nil {
//		return "", err
//	}
//	return search(ctx, query.Get(args), limit.Get(args))
//
// Arguments are kept as raw JSON until a handle reads them, so numbers preserve
// their full 64-bit range instead of being rounded through float64.
type Args struct {
	values   map[string]jsontext.Value
	declared map[string]argKind
}

// Has reports whether the model sent a value for name. Prefer the typed
// handle's Present, which cannot misspell the name and needs no type lookup.
func (a Args) Has(name string) bool {
	a.declare(name)
	_, ok := a.values[name]
	return ok
}

// Raw returns the argument decoded as a generic JSON value, or nil when it was
// omitted. Numbers are float64; use a typed handle when precision matters.
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

// anyPresent reports whether at least one of names was sent.
func (a Args) anyPresent(names []string) bool {
	for _, name := range names {
		if _, ok := a.values[name]; ok {
			return true
		}
	}
	return false
}

func (a Args) declare(name string) argKind {
	kind, ok := a.declared[name]
	if !ok {
		panic(fmt.Sprintf("loom: tool argument %q is not declared", name))
	}
	return kind
}

// argKind is the closed set of argument shapes the declared-argument API
// supports. It is deliberately small: each kind maps to exactly one JSON Schema
// type and one typed handle, so an argument can never be declared as one shape
// and read back as another.
type argKind uint8

const (
	argKindString argKind = iota
	argKindUint
	argKindFloat
	argKindBool
	argKindStrings
)

func (k argKind) String() string {
	switch k {
	case argKindString:
		return "string"
	case argKindUint:
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

// decodeHint describes the Go type a kind accepts, for a model-facing
// diagnostic when a value passes the schema but does not fit the Go type.
func (k argKind) decodeHint() string {
	switch k {
	case argKindString:
		return "a string"
	case argKindUint:
		return "an integer between 0 and 18446744073709551615"
	case argKindFloat:
		return "a number"
	case argKindBool:
		return "a boolean"
	case argKindStrings:
		return "an array of strings"
	default:
		return "a supported value"
	}
}
