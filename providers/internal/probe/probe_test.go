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

// A builder whose value cannot be serialized, or does not serialize to an object, is a mistake
// in the caller's adapter rather than something to report as "no parameters were sent".
func TestRequestParametersReportsWhatItCannotRead(t *testing.T) {
	if _, err := RequestParameters(func(loom.ChatRequest) (chan int, error) { return nil, nil }, loom.Reasoning{}); err == nil {
		t.Fatal("a builder value that cannot be marshaled must fail")
	}
	if _, err := RequestParameters(func(loom.ChatRequest) (int, error) { return 5, nil }, loom.Reasoning{}); err == nil {
		t.Fatal("a builder value that is not an object must fail")
	}
	// A body that holds none of the fields under test is reported as empty, not as an error.
	params, err := RequestParameters(func(loom.ChatRequest) (map[string]any, error) {
		return map[string]any{"model": "m", "messages": []any{}}, nil
	}, loom.Reasoning{})
	if err != nil || len(params) != 0 {
		t.Fatalf("params = %+v (%v)", params, err)
	}
}
