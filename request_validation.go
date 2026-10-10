package loom

import "errors"

// ErrMissingUserMessage means a chat context contains no user message.
// Callers must provide the task explicitly; Loom never invents a user message
// or changes a system/assistant/tool message's role.
var ErrMissingUserMessage = errors.New("loom: chat request requires at least one user message")

// ValidateChatRequest checks provider-independent chat input before any request.
// A user message anywhere in the context is sufficient: assistant continuations
// and tool-result histories do not have to end with a user message.
func ValidateChatRequest(req ChatRequest) error {
	for _, message := range req.Messages {
		if message.Role == RoleUser {
			return nil
		}
	}
	return ErrMissingUserMessage
}
