package prompttemplate

import "testing"

func TestValidateExactlyOnce(t *testing.T) {
	tests := []struct {
		name    string
		prompt  string
		wantErr bool
	}{
		{name: "once", prompt: "用户输入：{{user_input}}", wantErr: false},
		{name: "missing", prompt: "用户输入：", wantErr: true},
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
	got, err := RenderExactlyOnce("翻译：{{user_input}}", UserInputVariable, "hello", "system_prompt")
	if err != nil {
		t.Fatalf("RenderExactlyOnce: %v", err)
	}
	if got != "翻译：hello" {
		t.Fatalf("rendered = %q", got)
	}
}

func TestValidateAllExactlyOnce(t *testing.T) {
	err := ValidateAllExactlyOnce(
		"上下文：{{conversation_context}}\n问题：{{user_input}}\n回答：{{assistant_answer}}",
		"followups system_prompt",
		ConversationContextVariable,
		UserInputVariable,
		AssistantAnswerVariable,
	)
	if err != nil {
		t.Fatalf("ValidateAllExactlyOnce() unexpected error: %v", err)
	}

	err = ValidateAllExactlyOnce(
		"问题：{{user_input}}\n回答：{{assistant_answer}}",
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
		"上下文：{{conversation_context}}\n问题：{{user_input}}\n回答：{{assistant_answer}}",
		"followups system_prompt",
		map[string]string{
			ConversationContextVariable: "前文",
			UserInputVariable:           "用户问题",
			AssistantAnswerVariable:     "助手回答",
		},
		ConversationContextVariable,
		UserInputVariable,
		AssistantAnswerVariable,
	)
	if err != nil {
		t.Fatalf("RenderAllExactlyOnce(): %v", err)
	}
	want := "上下文：前文\n问题：用户问题\n回答：助手回答"
	if got != want {
		t.Fatalf("RenderAllExactlyOnce() = %q, want %q", got, want)
	}
}
