package contextpolicy

import (
	"context"
	"testing"

	"github.com/loomagent/loom"
)

func TestChain(t *testing.T) {
	appendMessage := func(content string) Policy {
		return Func(func(_ context.Context, input Input) (Result, error) {
			return Result{
				Messages:  append(input.Messages, loom.Message{Role: loom.RoleSystem, Content: content}),
				Decisions: []Decision{{Policy: content, Action: "append"}},
			}, nil
		})
	}
	result, err := (Chain{appendMessage("one"), appendMessage("two")}).Build(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 || result.Messages[1].Content != "two" || len(result.Decisions) != 2 {
		t.Fatalf("result = %+v", result)
	}
}
