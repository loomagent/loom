package calculator

import (
	"context"
	jsonv2 "encoding/json/v2"
	"testing"
)

func TestEvaluate(t *testing.T) {
	tests := map[string]string{
		"(2 + 3) * 4":    "20",
		"math.sqrt(144)": "12.0",
		"math.pow(2, 8)": "256.0",
	}
	for expression, want := range tests {
		got, err := Evaluate(context.Background(), expression)
		if err != nil {
			t.Fatalf("Evaluate(%q): %v", expression, err)
		}
		if got != want {
			t.Fatalf("Evaluate(%q) = %q, want %q", expression, got, want)
		}
	}
}

func TestTool(t *testing.T) {
	tool := New()
	out, err := tool.Invoke(context.Background(), `{"expression":"6 * 7"}`)
	if err != nil {
		t.Fatal(err)
	}
	var got response
	if err := jsonv2.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Expression != "6 * 7" || got.Result != "42" {
		t.Fatalf("response = %+v", got)
	}
}

func TestToolRejectsEmptyExpression(t *testing.T) {
	if _, err := New().Invoke(context.Background(), `{"expression":" "}`); err == nil {
		t.Fatal("expected error")
	}
}

// The expression comes from a model, so what the environment refuses matters as much as
// what it computes: statements, loops, definitions, loading, and recursion are all off.
func TestEvaluateRefusesWhatTheSandboxIsFor(t *testing.T) {
	refused := map[string]string{
		"syntax error":       "2 +",
		"unknown name":       "definitely_not_defined",
		"assignment":         "x = 1",
		"while loop":         "while True:\n  pass",
		"for loop":           "for x in [1]:\n  pass",
		"function":           "def f():\n  return 1",
		"load":               `load("other", "thing")`,
		"recursion":          "def f(n):\n  return f(n - 1)\nf(1)",
		"attribute escape":   "math.sqrt.__class__",
		"chained comparison": "1 < 2 < 3",
		// Starlark has no ** operator; a model that writes it is told so, and math.pow is
		// the way. The tool documents Starlark syntax for exactly this reason.
		"python power operator": "2 ** 3",
	}
	for name, expression := range refused {
		t.Run(name, func(t *testing.T) {
			if got, err := Evaluate(context.Background(), expression); err == nil {
				t.Fatalf("Evaluate(%q) = %q, want a refusal", expression, got)
			}
		})
	}
}

// Whatever it does compute comes back in its Starlark spelling, which is what the model
// reads next.
func TestEvaluateReturnsStarlarkValues(t *testing.T) {
	for expression, want := range map[string]string{
		"1 / 2":            "0.5",
		"math.pow(2, 10)":  "1024.0",
		"'text'":           `"text"`,
		"[1, 2]":           "[1, 2]",
		"True":             "True",
		"math.pi > 3":      "True",
		"abs(-3)":          "3",
		"1 if True else 2": "1",
		// A function value is a value: the environment allows defining one and only
		// forbids recursion, so calling it is an ordinary call.
		"(lambda x: x)(1)": "1",
	} {
		got, err := Evaluate(context.Background(), expression)
		if err != nil {
			t.Fatalf("Evaluate(%q): %v", expression, err)
		}
		if got != want {
			t.Errorf("Evaluate(%q) = %q, want %q", expression, got, want)
		}
	}
}

// The tool reports the expression as the caller wrote it, without the padding, and a
// failure reaches the caller as an error rather than as a result.
func TestToolTrimsTheExpressionAndReportsFailures(t *testing.T) {
	tool := New()
	out, err := tool.Invoke(context.Background(), `{"expression":"  math.pow(2, 10)  "}`)
	if err != nil {
		t.Fatal(err)
	}
	var got response
	if err := jsonv2.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Expression != "math.pow(2, 10)" || got.Result != "1024.0" {
		t.Fatalf("response = %+v", got)
	}

	if _, err := tool.Invoke(context.Background(), `{"expression":"2 +"}`); err == nil {
		t.Fatal("a broken expression must fail")
	}
}

// The tool a model sees: its name, its description, and the argument contract behind it.
func TestToolMetadata(t *testing.T) {
	info, err := New().Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != ToolName || info.Description == "" {
		t.Fatalf("info = %+v", info)
	}
	if info.Parameters == nil || info.Parameters.Properties["expression"] == nil {
		t.Fatalf("parameters = %+v", info.Parameters)
	}
}
