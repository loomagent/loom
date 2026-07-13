package calculator

import (
	"context"
	"encoding/json"
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
	if err := json.Unmarshal([]byte(out), &got); err != nil {
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
