package prompttemplate

import (
	"strings"
	"testing"
)

func TestValidateExactlyOnce(t *testing.T) {
	tests := []struct {
		name    string
		prompt  string
		wantErr bool
	}{
		{name: "once", prompt: "User input: {{user_input}}", wantErr: false},
		{name: "missing", prompt: "User input:", wantErr: true},
		{name: "duplicate", prompt: "{{user_input}}\n{{user_input}}", wantErr: true},
		{name: "empty", prompt: " ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateExactlyOnce(tt.prompt, UserInputVariable, "system_prompt")
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateExactlyOnce() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRenderExactlyOnce(t *testing.T) {
	got, err := RenderExactlyOnce("Translate: {{user_input}}", UserInputVariable, "hello", "system_prompt")
	if err != nil {
		t.Fatalf("RenderExactlyOnce: %v", err)
	}
	if got != "Translate: hello" {
		t.Fatalf("rendered = %q", got)
	}
}

func TestValidateAllExactlyOnce(t *testing.T) {
	err := ValidateAllExactlyOnce(
		"Context: {{conversation_context}}\nQuestion: {{user_input}}\nAnswer: {{assistant_answer}}",
		"followups system_prompt",
		ConversationContextVariable,
		UserInputVariable,
		AssistantAnswerVariable,
	)
	if err != nil {
		t.Fatalf("ValidateAllExactlyOnce() unexpected error: %v", err)
	}

	err = ValidateAllExactlyOnce(
		"Question: {{user_input}}\nAnswer: {{assistant_answer}}",
		"followups system_prompt",
		ConversationContextVariable,
		UserInputVariable,
		AssistantAnswerVariable,
	)
	if err == nil {
		t.Fatal("ValidateAllExactlyOnce() error = nil, want missing variable error")
	}
}

func TestRenderAllExactlyOnce(t *testing.T) {
	got, err := RenderAllExactlyOnce(
		"Context: {{conversation_context}}\nQuestion: {{user_input}}\nAnswer: {{assistant_answer}}",
		"followups system_prompt",
		map[string]string{
			ConversationContextVariable: "previous text",
			UserInputVariable:           "the user question",
			AssistantAnswerVariable:     "the assistant answer",
		},
		ConversationContextVariable,
		UserInputVariable,
		AssistantAnswerVariable,
	)
	if err != nil {
		t.Fatalf("RenderAllExactlyOnce(): %v", err)
	}
	want := "Context: previous text\nQuestion: the user question\nAnswer: the assistant answer"
	if got != want {
		t.Fatalf("RenderAllExactlyOnce() = %q, want %q", got, want)
	}
}

// A template that is missing a placeholder is rejected before it is rendered, and the message
// names the variable so the author can find it.
func TestValidateAllExactlyOnceReportsWhatIsWrong(t *testing.T) {
	tests := map[string]struct {
		template  string
		variables []string
		want      string
	}{
		"empty":          {"   ", []string{UserInputVariable}, "must not be empty"},
		"missing":        {"a prompt with no placeholder", []string{UserInputVariable}, "must contain exactly one"},
		"duplicated":     {"{{user_input}} and {{user_input}}", []string{UserInputVariable}, "must contain exactly one"},
		"later variable": {"{{user_input}}", []string{UserInputVariable, AssistantAnswerVariable}, "must contain exactly one"},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateAllExactlyOnce(testCase.template, "prompt", testCase.variables...)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want one containing %q", err, testCase.want)
			}
		})
	}
	if err := ValidateAllExactlyOnce("{{user_input}} {{assistant_answer}}", "prompt", UserInputVariable, AssistantAnswerVariable); err != nil {
		t.Fatalf("a valid template failed: %v", err)
	}
	// No variables is a template that has nothing to require.
	if err := ValidateAllExactlyOnce("plain", "prompt"); err != nil {
		t.Fatalf("a template without variables failed: %v", err)
	}
}

// Rendering refuses what validation refuses rather than rendering a half-filled template, and
// both the template and the value lose their surrounding whitespace.
func TestRenderingTrimsAndRefuses(t *testing.T) {
	if _, err := RenderExactlyOnce("", UserInputVariable, "value", "prompt"); err == nil {
		t.Fatal("an empty template must fail")
	}
	if _, err := RenderExactlyOnce("no placeholder", UserInputVariable, "value", "prompt"); err == nil {
		t.Fatal("a missing placeholder must fail")
	}
	if _, err := RenderAllExactlyOnce("no placeholder", "prompt", map[string]string{UserInputVariable: "value"}, UserInputVariable); err == nil {
		t.Fatal("a missing placeholder must fail")
	}

	got, err := RenderExactlyOnce("  {{user_input}}  ", UserInputVariable, "  spaced  ", "prompt")
	if err != nil || got != "spaced" {
		t.Fatalf("RenderExactlyOnce = %q, %v", got, err)
	}
	got, err = RenderAllExactlyOnce(" {{user_input}}/{{assistant_answer}} ", "prompt",
		map[string]string{UserInputVariable: " q ", AssistantAnswerVariable: " a "},
		UserInputVariable, AssistantAnswerVariable)
	if err != nil || got != "q/a" {
		t.Fatalf("RenderAllExactlyOnce = %q, %v", got, err)
	}
	// A variable the caller forgot to supply renders as empty rather than leaving the
	// placeholder in front of the model.
	got, err = RenderAllExactlyOnce("{{user_input}}!", "prompt", map[string]string{}, UserInputVariable)
	if err != nil || got != "!" {
		t.Fatalf("RenderAllExactlyOnce = %q, %v", got, err)
	}
}
