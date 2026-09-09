package loom

import (
	"fmt"
	"strings"
)

// ValidateModelReasoningCapabilities validates the caller's exact declaration.
// There is no built-in model catalogue or alias conversion. Administrators own
// the accuracy of their declarations; providers transmit their values unchanged.
func ValidateModelReasoningCapabilities(provider, model string, caps ModelCapabilities) error {
	if caps.reasoningProbe {
		return nil
	}
	switch caps.Reasoning {
	case "", ReasoningSupportNone, ReasoningSupportToggleable, ReasoningSupportAlwaysOn, ReasoningSupportToggleableDefaultOn, ReasoningSupportToggleableDefaultOff:
	default:
		return fmt.Errorf("loom: unknown reasoning capability %q", caps.Reasoning)
	}
	seen := map[ReasoningEffort]bool{}
	for _, effort := range caps.ReasoningEfforts {
		if !ValidReasoningEffort(effort) {
			return fmt.Errorf("loom: invalid declared reasoning effort %q", effort)
		}
		if seen[effort] {
			return fmt.Errorf("loom: duplicate declared reasoning effort %q", effort)
		}
		seen[effort] = true
	}
	if caps.Reasoning == ReasoningSupportNone && len(caps.ReasoningEfforts) > 0 {
		return fmt.Errorf("loom: reasoning=none cannot declare reasoning efforts")
	}
	return nil
}

// ValidReasoningEffort checks transport-safe identifiers without normalization.
func ValidReasoningEffort(effort ReasoningEffort) bool {
	if effort == "" || len(effort) > 64 {
		return false
	}
	for _, r := range effort {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// ResolveModelReasoning uses only the explicitly supplied model capabilities.
func ResolveModelReasoning(provider, model string, caps ModelCapabilities, r Reasoning) (ResolvedReasoning, error) {
	if err := ValidateModelReasoningCapabilities(provider, model, caps); err != nil {
		return ResolvedReasoning{}, err
	}
	return ResolveReasoning(caps, r)
}

// ReasoningProbeCapabilities constructs diagnostic-only synthetic capabilities.
// omitParameters observes provider defaults; false sends explicit raw requests.
// Keep these models isolated from business configuration. In particular, accepted
// alias requests do not establish independent native efforts.
func ReasoningProbeCapabilities(omitParameters bool) ModelCapabilities {
	c := ModelCapabilities{reasoningProbe: true}
	if omitParameters {
		c.Reasoning = ReasoningSupportNone
	}
	return c
}

// RequestValidationError identifies an adapter-local request rejection. Probes
// must never count it as evidence of server-side capability rejection.
type RequestValidationError struct{ Err error }

func (e *RequestValidationError) Error() string {
	return "loom: local request validation: " + e.Err.Error()
}
func (e *RequestValidationError) Unwrap() error { return e.Err }

// LocalRequestError annotates an error produced before any provider request.
func LocalRequestError(err error) error {
	if err == nil {
		return nil
	}
	return &RequestValidationError{Err: err}
}

// SplitModelName splits ChatModel.Name at its provider boundary; model IDs may
// contain further slashes (for example openrouter/openai/gpt-5).
func SplitModelName(name string) (provider, model string) {
	provider, model, _ = strings.Cut(name, "/")
	return
}
