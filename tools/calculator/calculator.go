// Package calculator provides a sandboxed mathematical expression tool.
package calculator

import (
	"context"
	jsonv2 "encoding/json/v2"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.starlark.net/lib/math"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/loomagent/loom"
)

// ToolName is the name exposed to models.
const ToolName = "calculator"

type response struct {
	Expression string `json:"expression"`
	Result     string `json:"result"`
}

// New constructs a calculator Loom tool.
func New() loom.Tool {
	description := "Evaluate a mathematical expression in a restricted Starlark environment. Supports arithmetic, parentheses, and functions from the Starlark math module."
	expression := loom.String("expression").
		Required().
		MinLen(1).
		NotBlank().
		Example("(2 + 3) * 4").
		Desc("Mathematical expression using Starlark syntax, for example '(2 + 3) * 4', 'math.sqrt(144)', or 'math.pow(2, 8)'.")
	contract := loom.MustArgsContract(ToolName, expression)
	return loom.NewArgsTool(contract, description, func(ctx context.Context, args loom.Args) (string, error) {
		return invoke(ctx, expression.Get(args))
	})
}

func invoke(ctx context.Context, rawExpression string) (string, error) {
	ctx, span := otel.Tracer("github.com/loomagent/loom/tools/calculator").Start(ctx, "calculator.evaluate")
	defer span.End()

	expression := strings.TrimSpace(rawExpression)
	span.SetAttributes(attribute.String("calculator.expression", expression))

	result, err := Evaluate(ctx, expression)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	span.SetAttributes(attribute.String("calculator.result", result))

	out, err := jsonv2.Marshal(response{Expression: expression, Result: result})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("calculator: marshal result: %w", err)
	}
	span.SetStatus(codes.Ok, "")
	return string(out), nil
}

// Evaluate evaluates expression in a restricted Starlark environment. Only the
// math module is available; statements, loops, mutation, loading, and recursion
// are disabled.
func Evaluate(_ context.Context, expression string) (string, error) {
	value, err := starlark.EvalOptions(
		&syntax.FileOptions{
			Set:               false,
			While:             false,
			TopLevelControl:   false,
			GlobalReassign:    false,
			LoadBindsGlobally: false,
			Recursion:         false,
		},
		&starlark.Thread{Name: ToolName},
		"expression",
		expression,
		starlark.StringDict{"math": math.Module},
	)
	if err != nil {
		return "", fmt.Errorf("calculator: evaluate: %w", err)
	}
	return value.String(), nil
}
