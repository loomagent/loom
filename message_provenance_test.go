package loom

import "testing"

func TestMessageProvenanceSeparatesTaskAndFrameworkUserRoles(t *testing.T) {
	terminal := NewTerminalUserMessage("question")
	scheduled := NewTaskUserMessage(MessageSourceScheduledTask, "scheduled question")
	status := NewFrameworkUserMessage(MessagePurposeRuntimeStatus, "same role, internal state")

	if terminal.Role != RoleUser || !terminal.IsExternalUserMessage() || terminal.Source() != MessageSourceTerminalUser {
		t.Fatalf("terminal message provenance = %#v", terminal)
	}
	if !scheduled.IsExternalUserMessage() || scheduled.Source() != MessageSourceScheduledTask {
		t.Fatalf("scheduled message provenance = %#v", scheduled)
	}
	if status.Role != RoleUser || status.IsExternalUserMessage() || !status.IsFrameworkUserMessage(MessagePurposeRuntimeStatus) {
		t.Fatalf("framework message provenance = %#v", status)
	}
	if status.Name != "" {
		t.Fatalf("internal provenance must not use provider-facing Name: %#v", status)
	}
}

func TestUntaggedUserMessageIsNotTrustedAsExternal(t *testing.T) {
	legacy := Message{Role: RoleUser, Content: "legacy question"}
	if legacy.IsExternalUserMessage() || legacy.Source() != MessageSourceUnknown {
		t.Fatalf("untagged user message must not be trusted as external: %#v", legacy)
	}
}
