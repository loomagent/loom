// Package calculator provides a sandboxed mathematical expression tool.
package calculator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
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

type request struct {
	Expression string `json:"expression"`
}

type response struct {
	Expression string `json:"expression"`
	Result     string `json:"result"`
}

// New constructs a calculator Loom tool.
func New() loom.Tool {
	params := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"expression": {
				Type:        "string",
				Description: "Mathematical expression using Starlark syntax, for example '(2 + 3) * 4', 'math.sqrt(144)', or 'math.pow(2, 8)'.",
			},
		},
		Required: []string{"expression"},
	}
	description := "Evaluate a mathematical expression in a restricted Starlark environment. Supports arithmetic, parentheses, and functions from the Starlark math module."
	return loom.NewTool(ToolName, description, params, invoke)
}

func invoke(ctx context.Context, argumentsJSON string) (string, error) {
	ctx, span := otel.Tracer("github.com/loomagent/loom/tools/calculator").Start(ctx, "calculator.evaluate")
	defer span.End()

	var input request
	if err := json.Unmarshal([]byte(argumentsJSON), &input); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("calculator: parse arguments: %w", err)
	}
	input.Expression = strings.TrimSpace(input.Expression)
	if input.Expression == "" {
		err := fmt.Errorf("calculator: expression is required")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	span.SetAttributes(attribute.String("calculator.expression", input.Expression))

	result, err := Evaluate(ctx, input.Expression)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	span.SetAttributes(attribute.String("calculator.result", result))

	out, err := json.Marshal(response{Expression: input.Expression, Result: result})
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
