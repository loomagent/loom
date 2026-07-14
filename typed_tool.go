package loom

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// TypedInvokeFunc is a tool implementation whose arguments have already been
// parsed and validated against a ToolContract.
type TypedInvokeFunc[T any] func(ctx context.Context, arguments T) (string, error)

// ToolContract binds one public tool name to a Go argument type and its compiled
// JSON Schema. A contract is immutable after construction and safe for
// concurrent Decode calls.
type ToolContract[T any] struct {
	name     string
	schema   *jsonschema.Schema
	resolved *jsonschema.Resolved
	expected string
}

type toolContractConfig struct {
	schema *jsonschema.Schema
}

// ToolContractOption configures a ToolContract before its schema is compiled.
type ToolContractOption func(*toolContractConfig) error

// WithArgumentSchema replaces the struct-derived schema. The schema is cloned
// before use and must still describe T.
func WithArgumentSchema(schema *jsonschema.Schema) ToolContractOption {
	return func(config *toolContractConfig) error {
		if schema == nil {
			return fmt.Errorf("argument schema is nil")
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
		configure(property)
		return nil
	}
}

// NewToolContract derives, configures, and compiles the argument contract for
// toolName. Tool names are required so every validation error can identify the
// tool that rejected the call.
func NewToolContract[T any](toolName string, options ...ToolContractOption) (*ToolContract[T], error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, fmt.Errorf("loom: tool contract name is required")
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
	return &ToolContract[T]{
		name:     toolName,
		schema:   config.schema,
		resolved: resolved,
		expected: summarizeExpectedInput(config.schema),
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
func (c *ToolContract[T]) Schema() *jsonschema.Schema { return c.schema.CloneSchemas() }

// Decode parses and validates one tool call using the precompiled contract.
func (c *ToolContract[T]) Decode(argumentsJSON string) (T, error) {
	return decodeToolArguments[T](c.name, argumentsJSON, c.schema, c.resolved, c.expected)
}

// NewTypedTool exposes a typed handler using contract. The model-facing schema
// is cloned from the immutable contract; changing ToolInfo cannot change the
// validator used by Decode.
func NewTypedTool[T any](contract *ToolContract[T], description string, fn TypedInvokeFunc[T], options ...ToolOption) Tool {
	if contract == nil {
		panic("loom: typed tool contract is nil")
	}
	if fn == nil {
		panic(fmt.Sprintf("loom: typed tool %q invoke function is nil", contract.name))
	}
	return NewTool(contract.name, description, contract.Schema(), func(ctx context.Context, argumentsJSON string) (string, error) {
		arguments, err := contract.Decode(argumentsJSON)
		if err != nil {
			return "", err
		}
		return fn(ctx, arguments)
	}, options...)
}
