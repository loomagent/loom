package prompttemplate

import (
	"fmt"
	"strings"
)

const (
	UserInputVariable           = "{{user_input}}"
	AssistantAnswerVariable     = "{{assistant_answer}}"
	ConversationContextVariable = "{{conversation_context}}"
)

func Count(template string, variable string) int {
	return strings.Count(template, variable)
}

func ValidateExactlyOnce(template string, variable string, name string) error {
	template = strings.TrimSpace(template)
	if template == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if count := Count(template, variable); count != 1 {
		return fmt.Errorf("%s must contain exactly one %s", name, variable)
	}
	return nil
}

func ValidateAllExactlyOnce(template string, name string, variables ...string) error {
	template = strings.TrimSpace(template)
	if template == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	for _, variable := range variables {
		if count := Count(template, variable); count != 1 {
			return fmt.Errorf("%s must contain exactly one %s", name, variable)
		}
	}
	return nil
}

func RenderExactlyOnce(template string, variable string, value string, name string) (string, error) {
	template = strings.TrimSpace(template)
	if err := ValidateExactlyOnce(template, variable, name); err != nil {
		return "", err
	}
	return strings.ReplaceAll(template, variable, strings.TrimSpace(value)), nil
}

func RenderAllExactlyOnce(template string, name string, replacements map[string]string, variables ...string) (string, error) {
	template = strings.TrimSpace(template)
	if err := ValidateAllExactlyOnce(template, name, variables...); err != nil {
		return "", err
	}
	for _, variable := range variables {
		template = strings.ReplaceAll(template, variable, strings.TrimSpace(replacements[variable]))
	}
	return template, nil
}
