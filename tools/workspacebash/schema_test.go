package workspacebash

import (
	"slices"
	"testing"
)

func TestDefaultParametersComesFromCommandRequest(t *testing.T) {
	params := DefaultParameters("Run a read-only command.")
	if !slices.Equal(params.Required, []string{"command"}) {
		t.Fatalf("required = %v, want [command]", params.Required)
	}
	command := params.Properties["command"]
	if command == nil || command.Type != "string" {
		t.Fatalf("command schema = %+v", command)
	}
	if command.Description != "Run a read-only command." {
		t.Fatalf("command description = %q", command.Description)
	}
}
