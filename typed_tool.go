package loom

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// InvokeFunc is a tool implementation whose arguments have already been
// parsed and validated against a ToolContract.
type InvokeFunc[T any] func(ctx context.Context, arguments T) (string, error)

// NoArguments is the standard contract for tools that accept no arguments.
// Such tools still require the model to send the empty JSON object {}.
type NoArguments struct{}

// ToolContract binds one public tool name to a Go argument type and its compiled
// JSON Schema. A contract is immutable after construction and safe for
// concurrent Decode calls.
type ToolContract[T any] struct {
	name     string
	schema   *jsonschema.Schema
	resolved *jsonschema.Resolved
	guidance argumentGuidance
}

type toolContractConfig struct {
	schema     *jsonschema.Schema
	configured bool
}

// ToolContractOption configures a ToolContract before its schema is compiled.
type ToolContractOption func(*toolContractConfig) error

// WithArgumentSchema replaces the struct-derived schema wholesale. The schema is
// cloned before use and must still describe T.
//
// Replacing means exactly that: every constraint derived from T's validate tags
// is discarded along with anything earlier options configured, so this must come
// before other schema options — passing it later is rejected rather than
// silently undoing them. Prefer ConfigureArgumentSchema when the goal is to
// adjust the derived schema rather than supply a different one.
func WithArgumentSchema(schema *jsonschema.Schema) ToolContractOption {
	return func(config *toolContractConfig) error {
		if schema == nil {
			return fmt.Errorf("argument schema is nil")
		}
		if config.configured {
			return fmt.Errorf("WithArgumentSchema replaces the whole schema and must come before other schema options")
		}
		config.schema = schema.CloneSchemas()
		return nil
	}
}

// ConfigureArgumentSchema mutates the private struct-derived schema before it
// is resolved. Use this for constraints that depend on runtime configuration.
func ConfigureArgumentSchema(configure func(*jsonschema.Schema) error) ToolContractOption {
	return func(config *toolContractConfig) error {
		if configure == nil {
			return nil
		}
		config.configured = true
		return configure(config.schema)
	}
}

// WithArgumentDescription overrides one top-level argument description.
func WithArgumentDescription(name, description string) ToolContractOption {
	return configureArgumentProperty(name, func(property *jsonschema.Schema) {
		property.Description = description
	})
}

// WithArgumentMaximum overrides one top-level argument's inclusive maximum.
func WithArgumentMaximum(name string, maximum float64) ToolContractOption {
	return configureArgumentProperty(name, func(property *jsonschema.Schema) {
		value := maximum
		property.Maximum = &value
	})
}

func configureArgumentProperty(name string, configure func(*jsonschema.Schema)) ToolContractOption {
	name = strings.TrimSpace(name)
	return func(config *toolContractConfig) error {
		if name == "" {
			return fmt.Errorf("argument property name is empty")
		}
		property := config.schema.Properties[name]
		if property == nil {
			return fmt.Errorf("argument property %q does not exist", name)
		}
		config.configured = true
		configure(property)
		return nil
	}
}

// NewToolContract derives, configures, and compiles the argument contract for
// toolName. Tool names are required so every validation error can identify the
// tool that rejected the call.
func NewToolContract[T any](toolName string, options ...ToolContractOption) (*ToolContract[T], error) {
	if err := ValidateToolName(toolName); err != nil {
		return nil, fmt.Errorf("loom: invalid tool name %q: %w", toolName, err)
	}
	schema, err := SchemaFor[T]()
	if err != nil {
		return nil, fmt.Errorf("loom: build argument schema for tool %q: %w", toolName, err)
	}
	config := &toolContractConfig{schema: schema}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(config); err != nil {
			return nil, fmt.Errorf("loom: configure argument schema for tool %q: %w", toolName, err)
		}
	}
	resolved, err := config.schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("loom: resolve argument schema for tool %q: %w", toolName, err)
	}
	guidance, err := buildArgumentGuidance[T](config.schema, resolved)
	if err != nil {
		return nil, fmt.Errorf("loom: build argument guidance for tool %q: %w", toolName, err)
	}
	return &ToolContract[T]{
		name:     toolName,
		schema:   config.schema,
		resolved: resolved,
		guidance: guidance,
	}, nil
}

// MustToolContract is NewToolContract for statically declared tool contracts.
func MustToolContract[T any](toolName string, options ...ToolContractOption) *ToolContract[T] {
	contract, err := NewToolContract[T](toolName, options...)
	if err != nil {
		panic(err)
	}
	return contract
}

// Name returns the public tool name bound to the contract.
func (c *ToolContract[T]) Name() string { return c.name }

// Schema returns an independent copy of the model-facing argument schema.
// Callers may mutate the result freely: it shares no state with the schema the
// contract validates against.
func (c *ToolContract[T]) Schema() *jsonschema.Schema { return cloneSchema(c.schema) }

// cloneSchema deep-copies a schema.
//
// jsonschema.Schema.CloneSchemas only clones nested *Schema values; slices and
// pointers holding plain values — Required, Enum, Examples, Minimum, MaxLength
// and friends — stay shared with the original. That is not enough here: a
// contract hands its schema to callers that may normalize it in place, and any
// such write would reach straight into the schema Decode validates against,
// racing with concurrent calls. Marshalling through JSON is exact for a JSON
// Schema and leaves nothing aliased.
func cloneSchema(schema *jsonschema.Schema) *jsonschema.Schema {
	if schema == nil {
		return nil
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return schema.CloneSchemas()
	}
	var clone jsonschema.Schema
	if err := json.Unmarshal(data, &clone); err != nil {
		return schema.CloneSchemas()
	}
	return &clone
}

// Decode parses and validates one tool call using the precompiled contract.
func (c *ToolContract[T]) Decode(argumentsJSON string) (T, error) {
	return decodeToolArguments[T](c.name, argumentsJSON, c.schema, c.resolved, c.guidance)
}

// NewTool exposes a typed handler using contract. The model-facing schema
// is cloned from the immutable contract; changing ToolInfo cannot change the
// validator used by Decode.
func NewTool[T any](contract *ToolContract[T], description string, fn InvokeFunc[T], options ...ToolOption) Tool {
	if contract == nil {
		panic("loom: typed tool contract is nil")
	}
	if fn == nil {
		panic(fmt.Sprintf("loom: typed tool %q invoke function is nil", contract.name))
	}
	return newTool(contract.name, description, contract.Schema(), func(ctx context.Context, argumentsJSON string) (string, error) {
		arguments, err := contract.Decode(argumentsJSON)
		if err != nil {
			return "", err
		}
		return fn(ctx, arguments)
	}, options...)
}
