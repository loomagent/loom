package loom

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"strings"

	jsonv2 "encoding/json/v2"

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
	schema    *Schema
	validator *toolcontract.Validator
	order     []*argSpec
	declared  map[string]argKind
	whole     []wholeValidator
	guidance  argumentGuidance
}

// NewArgsContract builds and compiles the argument contract for toolName.
func NewArgsContract(toolName string, declarations ...Declaration) (*ArgsContract, error) {
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
	guidance, err := buildArgsGuidance(schema)
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
func MustArgsContract(toolName string, declarations ...Declaration) *ArgsContract {
	contract, err := NewArgsContract(toolName, declarations...)
	if err != nil {
		panic(err)
	}
	return contract
}

// Name returns the public tool name bound to the contract.
func (c *ArgsContract) Name() string { return c.name }

// Schema returns an independent copy of the model-facing argument schema.
func (c *ArgsContract) Schema() *Schema { return cloneSchema(c.schema) }

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
//     retry. A validator that returns anything other than Invalid/InvalidOn
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
	args := Args{values: values, declared: c.declared, raw: raw}
	// JSON Schema cannot express the exact bounds of a Go integer, so a value
	// that passes the schema may still not fit the declared handle. Check the
	// decode once here so a handle read never fails on model input.
	if issues := c.checkArgumentTypes(args); len(issues) > 0 {
		return Args{}, newArgumentTypeToolArgumentError(c.name, c.guidance, issues)
	}
	issues, err := c.runValidators(ctx, args)
	if err != nil {
		return Args{}, fmt.Errorf("loom: tool %q argument validators: %w", c.name, err)
	}
	if len(issues) > 0 {
		return Args{}, newCustomToolArgumentError(c.name, c.guidance, issues)
	}
	return args, nil
}

// checkArgumentTypes decodes every present argument into the Go type its handle
// reads, so a value the schema accepted but the handle cannot represent is a
// model-facing type problem instead of a panic on read.
func (c *ArgsContract) checkArgumentTypes(args Args) []ToolArgumentIssue {
	var issues []ToolArgumentIssue
	for _, spec := range c.order {
		raw, present := args.values[spec.name]
		if !present {
			continue
		}
		if err := spec.decodeInto(raw); err != nil {
			issues = append(issues, ToolArgumentIssue{
				Field:   spec.name,
				Rule:    "type",
				Message: quoteField(spec.name) + " must be " + spec.kind.decodeHint(),
			})
		}
	}
	return issues
}

// decodeInto reports whether raw decodes into the Go type the kind's handle
// reads. The target carries only its type.
func (s *argSpec) decodeInto(raw jsontext.Value) error {
	var target any
	switch s.kind {
	case argKindString:
		target = new(string)
	case argKindUint:
		target = new(uint64)
	case argKindFloat:
		target = new(float64)
	case argKindBool:
		target = new(bool)
	case argKindStrings:
		target = new([]string)
	}
	return jsonv2.Unmarshal(raw, target)
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
		// A whole-call rule is only meaningful once at least one of its
		// arguments is present; running it on an all-absent payload would
		// invent problems for arguments the model never sent.
		if !args.anyPresent(validate.fields) {
			continue
		}
		collected, fatal := classifyValidatorError(ctx, "", validate.fn(ctx, args))
		if fatal != nil {
			return nil, fatal
		}
		issues = append(issues, collected...)
	}
	if err := c.checkIssueFields(issues); err != nil {
		return nil, err
	}
	return issues, nil
}

// checkIssueFields rejects a validator that names an argument the contract does
// not declare. The issue field is model-facing, so a typo in InvalidAt would
// mislabel the problem instead of failing; treat it as an internal error so it
// surfaces in development rather than as a confusing correction request. A
// validator that has a handle should call InvalidOn instead and get the name
// checked by construction.
func (c *ArgsContract) checkIssueFields(issues []ToolArgumentIssue) error {
	for _, issue := range issues {
		if issue.Field == "" {
			continue
		}
		if _, ok := c.declared[issue.Field]; !ok {
			return fmt.Errorf("validator reported unknown argument %q", issue.Field)
		}
	}
	return nil
}

// schema projects the accumulated declarations into an object schema. Property
// order follows declaration order; additional properties are rejected so a
// model that invents an argument name is told rather than silently ignored.
func (b *argsBuilder) schema() *Schema {
	properties := make(map[string]*Schema, len(b.order))
	required := make([]string, 0, len(b.order))
	for _, spec := range b.order {
		properties[spec.name] = spec.schema()
		if spec.required {
			required = append(required, spec.name)
		}
	}
	return &Schema{
		Type:                 "object",
		Properties:           properties,
		Required:             required,
		AdditionalProperties: new(false),
	}
}

// schema maps one declaration onto its JSON Schema property.
func (s *argSpec) schema() *Schema {
	property := &Schema{Description: s.description, Examples: s.examples}
	switch s.kind {
	case argKindString:
		property.Type = "string"
		property.MinLength = s.minLength
		property.MaxLength = s.maxLength
		property.Enum = s.enum
		property.Format = s.format
		// A format is only worth advertising while something enforces it, so the shape
		// check it projects stays on the property itself.
		if pattern, ok := formatPatterns[s.format]; ok {
			property.Pattern = pattern
		}
		// Every other pattern source becomes its own allOf branch. A property carries one
		// pattern, so a value that is both a date and non-blank, or both a date and a
		// shape the author pinned down, needs two branches rather than one keyword that
		// quietly replaces the other.
		var additional []string
		if s.pattern != "" && s.pattern != property.Pattern {
			additional = append(additional, s.pattern)
		}
		if s.notBlank {
			additional = append(additional, notBlankPattern)
		}
		switch len(additional) {
		case 0:
			// The property's own pattern already says everything the author declared.
		case 1:
			if property.Pattern == "" {
				property.Pattern = additional[0]
				break
			}
			property.AllOf = []*Schema{{Pattern: additional[0]}}
		default:
			property.AllOf = make([]*Schema, 0, len(additional))
			for _, pattern := range additional {
				property.AllOf = append(property.AllOf, &Schema{Pattern: pattern})
			}
		}
	case argKindUint:
		property.Type = "integer"
		// A non-negative argument must reject negatives in the schema; JSON
		// Schema has no separate unsigned type, so this is the only place the
		// constraint can live.
		if s.minimum == nil {
			zero := 0.0
			property.Minimum = &zero
		} else {
			property.Minimum = s.minimum
		}
		property.Maximum = s.maximum
		property.ExclusiveMinimum = s.exclusiveMinimum
		property.ExclusiveMaximum = s.exclusiveMaximum
		property.Enum = s.enum
	case argKindFloat:
		property.Type = "number"
		property.Minimum = s.minimum
		property.Maximum = s.maximum
		property.ExclusiveMinimum = s.exclusiveMinimum
		property.ExclusiveMaximum = s.exclusiveMaximum
	case argKindBool:
		property.Type = "boolean"
	case argKindStrings:
		property.Type = "array"
		property.Items = &Schema{Type: "string"}
		property.MinItems = s.minItems
		property.MaxItems = s.maxItems
		property.UniqueItems = s.uniqueItems
	}
	return property
}

// buildArgsGuidance is buildArgumentGuidance without a Go type to decode into.
// Declared-argument contracts have no struct, so an assembled example is
// validated against the schema alone.
func buildArgsGuidance(schema *Schema) (argumentGuidance, error) {
	guidance := argumentGuidance{built: true, expected: summarizeExpectedArguments(schema)}
	if err := validateDeclaredExamples(schema, ""); err != nil {
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
	if err := ValidateSchema(schema, example); err != nil {
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
