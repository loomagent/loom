package probe

import (
	"errors"
	"testing"

	"github.com/loomagent/loom"
)

type probeBody struct {
	Thinking        string `json:"thinking,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	Model           string `json:"model"`
}

// Only the reasoning-related fields are evidence; the rest of the request is not
// reported, so a probe report stays about the parameter under test.
func TestRequestParametersKeepsOnlyReasoningFields(t *testing.T) {
	build := func(req loom.ChatRequest) (probeBody, error) {
		if len(req.Messages) != 1 || req.Messages[0].Role != loom.RoleUser || req.Messages[0].Content != "probe" {
			t.Fatalf("builder received %+v", req.Messages)
		}
		return probeBody{Thinking: "enabled", Model: "m"}, nil
	}
	got, err := RequestParameters(build, loom.Reasoning{Mode: loom.ReasoningModeEnabled})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["thinking"] != "enabled" {
		t.Fatalf("parameters = %+v", got)
	}
}

// A provider that cannot serialize its own request must not be reported as
// having sent nothing: the failure has to reach the caller.
func TestRequestParametersPropagatesBuilderFailure(t *testing.T) {
	sentinel := errors.New("cannot serialize")
	_, err := RequestParameters(func(loom.ChatRequest) (probeBody, error) {
		return probeBody{}, sentinel
	}, loom.Reasoning{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the builder's failure", err)
	}
}
