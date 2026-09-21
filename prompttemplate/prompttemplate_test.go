package prompttemplate

import "testing"

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
