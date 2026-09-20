package loom

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"strings"

	jsonv2 "encoding/json/v2"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/loomagent/loom/internal/toolcontract"
)

// ArgsContract binds one public tool name to a list of declared arguments and
// the JSON Schema derived from them. A contract is immutable after construction
// and safe for concurrent Decode calls.
//
// It is the declared-argument counterpart to ToolContract: where ToolContract
// derives its schema from a Go struct, an ArgsContract is written as one flat
// list of argument declarations, so the name, type, description, required flag,
// constraints, and per-field validation of every argument live together:
//
//	contract := loom.MustArgsContract("web_search",
//	    loom.String("query").Required().MinLen(1).Desc("Search query."),
//	    loom.Enum("type", "search", "news").Desc("Result type."),
//	    loom.Date("date_from").Desc("Optional ISO date lower bound."),
//	    loom.ValidateArgs(func(ctx context.Context, args loom.Args) error {
//	        if from, to := args.String("date_from"), args.String("date_to"); from > to {
//	            return loom.InvalidAt("date_to", "date_to must not precede date_from")
//	        }
//	        return nil
//	    }),
//	)
//
// Handlers read arguments through the typed Args getters, so no struct tags and
// no generated type are required.
type ArgsContract struct {
	name      string
	schema    *jsonschema.Schema
	validator *toolcontract.Validator
	order     []*argSpec
	declared  map[string]argKind
	whole     []func(ctx context.Context, args Args) error
	guidance  argumentGuidance
}

// NewArgsContract builds and compiles the argument contract for toolName.
func NewArgsContract(toolName string, declarations ...Arg) (*ArgsContract, error) {
	if err := ValidateToolName(toolName); err != nil {
		return nil, fmt.Errorf("loom: invalid tool name %q: %w", toolName, err)
	}
	builder := newArgsBuilder()
	for index, declaration := range declarations {
		if declaration == nil {
			return nil, fmt.Errorf("loom: tool %q has a nil argument declaration at index %d", toolName, index)
		}
		if err := declaration.declare(builder); err != nil {
			return nil, fmt.Errorf("loom: tool %q: %w", toolName, err)
		}
	}
	schema := builder.schema()
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("loom: resolve argument schema for tool %q: %w", toolName, err)
	}
	guidance, err := buildArgsGuidance(schema, resolved)
	if err != nil {
		return nil, fmt.Errorf("loom: build argument guidance for tool %q: %w", toolName, err)
	}
	validator, err := compileValidationSchema(schema)
	if err != nil {
		return nil, fmt.Errorf("loom: compile argument schema for tool %q: %w", toolName, err)
	}
	declared := make(map[string]argKind, len(builder.order))
	for _, spec := range builder.order {
		declared[spec.name] = spec.kind
	}
	return &ArgsContract{
		name:      toolName,
		schema:    schema,
		validator: validator,
		order:     builder.order,
		declared:  declared,
		whole:     builder.whole,
		guidance:  guidance,
	}, nil
}

// MustArgsContract is NewArgsContract for statically declared tool contracts.
// It panics when the declaration is invalid, which indicates a programming
// error in the declared contract.
func MustArgsContract(toolName string, declarations ...Arg) *ArgsContract {
	contract, err := NewArgsContract(toolName, declarations...)
	if err != nil {
		panic(err)
	}
	return contract
}

// Name returns the public tool name bound to the contract.
func (c *ArgsContract) Name() string { return c.name }

// Schema returns an independent copy of the model-facing argument schema.
func (c *ArgsContract) Schema() *jsonschema.Schema { return cloneSchema(c.schema) }

// Decode parses and validates one tool call with a background context.
func (c *ArgsContract) Decode(argumentsJSON string) (Args, error) {
	return c.DecodeContext(context.Background(), argumentsJSON)
}

// DecodeContext parses and validates one tool call using the precompiled
// contract. Validation runs in two layers:
//
//  1. JSON Schema, which enforces type, presence, enumeration, and the declared
//     range and format constraints. When it rejects the input, declared
//     validators do not run: they assume a well-shaped value, and running them
//     on the wrong shape would add noise rather than information.
//  2. Declared field and whole-call validators, which run over the schema-valid
//     value. They all run and their problems are collected, so the model
//     receives every business-rule violation in one turn instead of one per
//     retry. A validator that returns anything other than Invalid/InvalidAt
//     aborts the decode as an internal failure.
func (c *ArgsContract) DecodeContext(ctx context.Context, argumentsJSON string) (Args, error) {
	// Providers commonly send an empty string rather than "{}" when a model
	// calls a tool that takes no arguments, or none of its optional ones.
	if strings.TrimSpace(argumentsJSON) == "" {
		argumentsJSON = "{}"
	}
	raw, err := readStrictJSON(argumentsJSON)
	if err != nil {
		return Args{}, newJSONToolArgumentError(c.name, c.guidance, err)
	}
	if validationError := c.validator.Validate(raw); validationError != nil {
		return Args{}, newSchemaToolArgumentError(c.name, c.guidance, validationError)
	}
	var values map[string]jsontext.Value
	if err := jsonv2.Unmarshal(raw, &values); err != nil {
		return Args{}, newJSONToolArgumentError(c.name, c.guidance, err)
	}
	args := Args{values: values, declared: c.declared}
	issues, err := c.runValidators(ctx, args)
	if err != nil {
		return Args{}, fmt.Errorf("loom: tool %q argument validators: %w", c.name, err)
	}
	if len(issues) > 0 {
		return Args{}, newCustomToolArgumentError(c.name, c.guidance, issues)
	}
	return args, nil
}

// runValidators executes every declared validator and collects the model-facing
// problems they report. Field validators run in declaration order and only for
// arguments the model actually sent; whole-call validators run afterwards.
func (c *ArgsContract) runValidators(ctx context.Context, args Args) ([]ToolArgumentIssue, error) {
	var issues []ToolArgumentIssue
	for _, spec := range c.order {
		if len(spec.validators) == 0 {
			continue
		}
		value, present := args.values[spec.name]
		if !present {
			continue
		}
		for _, validate := range spec.validators {
			collected, fatal := classifyValidatorError(ctx, spec.name, validate(ctx, value))
			if fatal != nil {
				return nil, fatal
			}
			issues = append(issues, collected...)
		}
	}
	for _, validate := range c.whole {
		collected, fatal := classifyValidatorError(ctx, "", validate(ctx, args))
		if fatal != nil {
			return nil, fatal
		}
		issues = append(issues, collected...)
	}
	return issues, nil
}

// schema projects the accumulated declarations into an object schema. Property
// order follows declaration order; additional properties are rejected so a
// model that invents an argument name is told rather than silently ignored.
func (b *argsBuilder) schema() *jsonschema.Schema {
	properties := make(map[string]*jsonschema.Schema, len(b.order))
	required := make([]string, 0, len(b.order))
	for _, spec := range b.order {
		properties[spec.name] = spec.schema()
		if spec.required {
			required = append(required, spec.name)
		}
	}
	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           properties,
		Required:             required,
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
}

// schema maps one declaration onto its JSON Schema property.
func (s *argSpec) schema() *jsonschema.Schema {
	property := &jsonschema.Schema{Description: s.description, Examples: s.examples}
	switch s.kind {
	case argKindString:
		property.Type = "string"
		property.MinLength = s.minLength
		property.MaxLength = s.maxLength
		property.Enum = s.enum
		property.Format = s.format
		switch {
		case s.pattern != "":
			property.Pattern = s.pattern
		default:
			if pattern, ok := formatPatterns[s.format]; ok {
				property.Pattern = pattern
			}
		}
	case argKindInt:
		property.Type = "integer"
		property.Minimum = s.minimum
		property.Maximum = s.maximum
		property.Enum = s.enum
	case argKindFloat:
		property.Type = "number"
		property.Minimum = s.minimum
		property.Maximum = s.maximum
	case argKindBool:
		property.Type = "boolean"
	case argKindStrings:
		property.Type = "array"
		property.Items = &jsonschema.Schema{Type: "string"}
		property.MinItems = s.minItems
		property.MaxItems = s.maxItems
		property.UniqueItems = s.uniqueItems
	}
	return property
}

// buildArgsGuidance is buildArgumentGuidance without a Go type to decode into.
// Declared-argument contracts have no struct, so an assembled example is
// validated against the schema alone.
func buildArgsGuidance(schema *jsonschema.Schema, resolved *jsonschema.Resolved) (argumentGuidance, error) {
	guidance := argumentGuidance{built: true, expected: summarizeExpectedArguments(schema)}
	if err := validateDeclaredExamples(schema, schema, ""); err != nil {
		return argumentGuidance{}, err
	}
	example, complete, declared := buildSchemaExample(schema)
	if !complete {
		return guidance, nil
	}
	reject := func(format string, err error) (argumentGuidance, error) {
		if declared {
			return argumentGuidance{}, fmt.Errorf(format, err)
		}
		return guidance, nil
	}
	if err := resolved.Validate(example); err != nil {
		return reject("assembled example does not satisfy JSON Schema: %w", err)
	}
	data, err := jsonv2.Marshal(example)
	if err != nil {
		return reject("marshal assembled example: %w", err)
	}
	if len([]rune(string(data))) <= maxExampleArgumentRunes {
		guidance.example = string(data)
	}
	return guidance, nil
}

// NewArgsTool exposes a handler over a declared-argument contract. Arguments are
// validated by the contract before the handler runs, so handlers operate on a
// checked Args value.
func NewArgsTool(contract *ArgsContract, description string, fn func(ctx context.Context, args Args) (string, error), options ...ToolOption) Tool {
	if contract == nil {
		panic("loom: args tool contract is nil")
	}
	if fn == nil {
		panic(fmt.Sprintf("loom: args tool %q invoke function is nil", contract.name))
	}
	return newTool(contract.name, description, contract.Schema(), func(ctx context.Context, argumentsJSON string) (string, error) {
		arguments, err := contract.DecodeContext(ctx, argumentsJSON)
		if err != nil {
			return "", err
		}
		return fn(ctx, arguments)
	}, options...)
}
