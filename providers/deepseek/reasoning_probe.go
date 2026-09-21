package deepseek

import (
	"github.com/loomagent/loom"
	"github.com/loomagent/loom/providers/internal/probe"
)

// ReasoningRequestParameters exposes actual adapter serialization for diagnostic
// evidence. It does not send a request or infer upstream interpretation.
func (m *Model) ReasoningRequestParameters(r loom.Reasoning) (map[string]any, error) {
	return probe.RequestParameters(m.buildRequest, r)
}
