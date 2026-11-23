package loom

import (
	"errors"
	"fmt"
)

// ErrMissingUserMessage means a chat context contains no user message.
// Callers must provide the task explicitly; Loom never invents a user message
// or changes a system/assistant/tool message's role.
var ErrMissingUserMessage = errors.New("loom: chat request requires at least one user message")

// ValidateChatRequest checks provider-independent chat input before any request.
// A user message anywhere in the context is sufficient: assistant continuations
// and tool-result histories do not have to end with a user message.
//
// It also refuses a part of a kind no provider can send. Leaving such a part out
// would hand the model a message that quietly lacks something the caller wrote.
func ValidateChatRequest(req ChatRequest) error {
	hasUser := false
	for i, message := range req.Messages {
		for _, part := range message.Parts {
			switch part.Type {
			case ContentText, ContentImage:
			default:
				return fmt.Errorf("loom: message %d uses unknown content part type %q", i, part.Type)
			}
		}
		if message.Role == RoleUser {
			hasUser = true
		}
	}
	if !hasUser {
		return ErrMissingUserMessage
	}
	return nil
}
