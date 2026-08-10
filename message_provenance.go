package loom

// MessageSource classifies who produced a message inside the Agent runtime.
// It is intentionally independent from Role, which is the provider wire role.
type MessageSource string

const (
	MessageSourceUnknown       MessageSource = ""
	MessageSourceTerminalUser  MessageSource = "terminal_user"
	MessageSourceScheduledTask MessageSource = "scheduled_task"
	MessageSourceReport        MessageSource = "report"
	MessageSourceFramework     MessageSource = "framework"
)

// MessagePurpose classifies why an internally generated message exists.
type MessagePurpose string

const (
	MessagePurposeTask          MessagePurpose = "task"
	MessagePurposeRuntimeStatus MessagePurpose = "runtime_status"
	MessagePurposeRetryFeedback MessagePurpose = "retry_feedback"
)

func NewTerminalUserMessage(content string) Message {
	return NewTaskUserMessage(MessageSourceTerminalUser, content)
}

// NewTaskUserMessage constructs a persisted business task input. The source
// must come from durable provenance rather than being inferred from role=user.
func NewTaskUserMessage(source MessageSource, content string) Message {
	return Message{
		Role:    RoleUser,
		Content: content,
		source:  source,
		purpose: MessagePurposeTask,
	}
}

// NewFrameworkUserMessage constructs a framework-generated message that must
// use role=user at the provider boundary. Source and purpose remain local-only.
func NewFrameworkUserMessage(purpose MessagePurpose, content string) Message {
	return Message{
		Role:    RoleUser,
		Content: content,
		source:  MessageSourceFramework,
		purpose: purpose,
	}
}

func (m Message) Source() MessageSource   { return m.source }
func (m Message) Purpose() MessagePurpose { return m.purpose }

// IsExternalUserMessage accepts only explicitly tagged business task inputs.
func (m Message) IsExternalUserMessage() bool {
	if m.Role != RoleUser || m.purpose != MessagePurposeTask {
		return false
	}
	switch m.source {
	case MessageSourceTerminalUser, MessageSourceScheduledTask, MessageSourceReport:
		return true
	default:
		return false
	}
}

func (m Message) IsFrameworkUserMessage(purpose MessagePurpose) bool {
	return m.Role == RoleUser && m.source == MessageSourceFramework && m.purpose == purpose
}
