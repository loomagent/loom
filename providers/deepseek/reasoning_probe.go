package deepseek

import (
	jsonv2 "encoding/json/v2"

	"github.com/loomagent/loom"
)

// ReasoningRequestParameters exposes actual adapter serialization for diagnostic
// evidence. It does not send a request or infer upstream interpretation.
func (m *Model) ReasoningRequestParameters(r loom.Reasoning) (map[string]any, error) {
	request, err := m.buildRequest(loom.ChatRequest{Messages: []loom.Message{{Role: loom.RoleUser, Content: "probe"}}, Reasoning: r})
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
