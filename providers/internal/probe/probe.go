// Package probe holds the provider-side diagnostic serialization that
// modelprobe consumes through each provider's ReasoningRequestParameters.
//
// Providers expose the parameters an adapter actually serializes so a probe
// report can record evidence instead of an inference. The serialization is the
// same everywhere; what differs is each provider's request builder, which is why
// the builder arrives as an argument.
package probe

import (
	jsonv2 "encoding/json/v2"

	"github.com/loomagent/loom"
)

// RequestParameters serializes one probe request through build and returns only
// the reasoning-related fields it produced. It neither sends a request nor
// infers how the provider interprets the result.
func RequestParameters[T any](build func(loom.ChatRequest) (T, error), reasoning loom.Reasoning) (map[string]any, error) {
	request, err := build(loom.ChatRequest{
		Messages:  []loom.Message{{Role: loom.RoleUser, Content: "probe"}},
		Reasoning: reasoning,
	})
	if err != nil {
		return nil, err
	}
	data, err := jsonv2.Marshal(request)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := jsonv2.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, key := range []string{"thinking", "reasoning", "reasoning_effort"} {
		if value, ok := body[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}
