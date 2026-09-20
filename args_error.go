package loom

import (
	"context"
	"errors"
	"fmt"
)

// ToolArgumentErrorCustom marks tool arguments rejected by a declared field or
// whole-call validator rather than by the JSON Schema.
const ToolArgumentErrorCustom ToolArgumentErrorKind = "custom_validation"

// argumentProblem is a model-facing argument problem reported by a declared
// validator. It is deliberately separate from an ordinary error: a validator
// that returns Invalid is telling the model its input is wrong, while any other
// error is an internal failure that must reach the operator instead of the
// model.
type argumentProblem struct {
	field   string
	message string
}

func (p *argumentProblem) Error() string { return p.message }

// Invalid reports a model-facing problem with the current argument. The message
// is shown to the model, so it should say what to change, not merely what is
// wrong. Use it from StringArg.Validate and friends.
func Invalid(format string, args ...any) error {
	return &argumentProblem{message: fmt.Sprintf(format, args...)}
}

// InvalidOn is Invalid for the argument a handle names. The name comes from the
// handle, so it cannot drift from the declared argument.
func InvalidOn(handle Handle, format string, args ...any) error {
	return &argumentProblem{field: handle.argumentName(), message: fmt.Sprintf(format, args...)}
}

// InvalidAt is Invalid for a named argument. It is the string-keyed escape
// hatch; prefer InvalidOn, whose name cannot be misspelled.
func InvalidAt(field, format string, args ...any) error {
	return &argumentProblem{field: field, message: fmt.Sprintf(format, args...)}
}

// classifyValidatorError splits a validator's error into model-facing issues
// and an internal failure. A validator may report several problems by joining
// Invalid values with errors.Join. Any non-Invalid leaf is an internal failure
// and wins over the issues: infrastructure trouble must not be dressed up as a
// model mistake.
func classifyValidatorError(ctx context.Context, field string, err error) (issues []ToolArgumentIssue, fatal error) {
	if err == nil {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	problems, other := collectArgumentProblems(err)
	if other != nil {
		return nil, other
	}
	issues = make([]ToolArgumentIssue, 0, len(problems))
	for _, problem := range problems {
		name := problem.field
		if name == "" {
			name = field
		}
		issues = append(issues, ToolArgumentIssue{Field: name, Rule: "custom", Message: problem.message})
	}
	return issues, nil
}

// collectArgumentProblems walks the error tree, returning every *argumentProblem
// alongside any subtree that is not one. errors.Join reports its children via
// Unwrap() []error; errors.As only surfaces the first match, so the tree is
// walked explicitly.
func collectArgumentProblems(err error) (problems []*argumentProblem, other error) {
	if err == nil {
		return nil, nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			childProblems, childOther := collectArgumentProblems(child)
			problems = append(problems, childProblems...)
			if childOther != nil {
				other = errors.Join(other, childOther)
			}
		}
		return problems, other
	}
	if problem, ok := err.(*argumentProblem); ok {
		return []*argumentProblem{problem}, nil
	}
	return nil, err
}

func newCustomToolArgumentError(tool string, guidance argumentGuidance, issues []ToolArgumentIssue) error {
	return &ToolArgumentError{
		Tool:              tool,
		Kind:              ToolArgumentErrorCustom,
		Issues:            clampIssues(issues),
		ExpectedArguments: guidance.expected,
		ExampleArguments:  guidance.example,
		Err:               errCustomArgumentValidation,
	}
}

// newArgumentTypeToolArgumentError reports a value that passed JSON Schema but
// does not fit the Go type its handle reads, such as an integer beyond the
// unsigned range.
func newArgumentTypeToolArgumentError(tool string, guidance argumentGuidance, issues []ToolArgumentIssue) error {
	return &ToolArgumentError{
		Tool:              tool,
		Kind:              ToolArgumentErrorSchema,
		Issues:            clampIssues(issues),
		ExpectedArguments: guidance.expected,
		ExampleArguments:  guidance.example,
		Err:               errArgumentType,
	}
}

var (
	errCustomArgumentValidation = errors.New("tool argument validation failed")
	errArgumentType             = errors.New("tool argument does not fit its declared type")
)
