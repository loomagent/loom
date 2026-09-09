package loom

import (
	"fmt"
	"slices"
	"strings"
)

// ReasoningContract describes model-level effort semantics, not a default
// business configuration. Efforts excludes aliases and off values. A declaration
// is evidence of native semantics, never proof of experimentally distinct budgets.
// Source and Match make the scope of that evidence available to callers and UIs.
type ReasoningContract struct {
	Provider   string                              `json:"provider"`
	Model      string                              `json:"model"`
	Match      string                              `json:"match"`
	Efforts    []ReasoningEffort                   `json:"efforts"`
	Aliases    map[ReasoningEffort]ReasoningEffort `json:"aliases"`
	OffAliases []ReasoningEffort                   `json:"off_aliases"`
	Source     string                              `json:"source"`
	SourceURLs []string                            `json:"source_urls"`
}

// ValidateEffort rejects aliases rather than silently normalizing them. The empty
// effort is not an option; it is handled separately by explicit mode validation.
func (c ReasoningContract) ValidateEffort(effort ReasoningEffort) error {
	if target, ok := c.Aliases[effort]; ok {
		return fmt.Errorf("loom: %s/%s reasoning effort %q is an alias of %q; explicitly select %q", c.Provider, c.Model, effort, target, target)
	}
	if slices.Contains(c.OffAliases, effort) {
		return fmt.Errorf("loom: %s/%s reasoning effort %q disables reasoning; explicitly select Mode=disabled with an empty effort", c.Provider, c.Model, effort)
	}
	if !slices.Contains(c.Efforts, effort) {
		return fmt.Errorf("loom: %s/%s unsupported reasoning effort %q (canonical efforts: %v)", c.Provider, c.Model, effort, c.Efforts)
	}
	return nil
}

// LookupReasoningContract is an offline, provider+exact-model lookup. Unknown
// endpoints, latest-version aliases, and future releases require a manual
// declaration: their semantics must not be inferred from a name prefix.
// Source scope follows official documentation reviewed for
// wolotech/product-issues#100 on 2026-09-09. Returned collections are fresh copies.
func LookupReasoningContract(provider, model string) (ReasoningContract, bool) {
	if provider != "ark" {
		return ReasoningContract{}, false
	}
	c := ReasoningContract{Provider: provider, Model: model, Match: model,
		Source: "official_documentation", SourceURLs: []string{
			"https://ark.volcengine.com/region:cn-beijing/docs/82379/1449737?lang=zh",
			"https://docs.volcengine.com/docs/82379/1494384?lang=zh",
		}, OffAliases: []ReasoningEffort{"none", "minimal"},
	}
	switch model {
	case "doubao-seed-evolving",
		"doubao-seed-2-1-pro-260628", "doubao-seed-2-1-turbo-260628",
		"doubao-seed-2-0-lite-260428", "doubao-seed-2-0-mini-260428",
		"doubao-seed-2-0-pro-260215", "doubao-seed-2-0-lite-260215",
		"doubao-seed-2-0-mini-260215", "doubao-seed-2-0-code-preview-260215",
		"doubao-seed-1-8-251228", "doubao-seed-1-6-251015", "doubao-seed-character-260628":
		c.Efforts = []ReasoningEffort{"low", "medium", "high"}
		c.Aliases = map[ReasoningEffort]ReasoningEffort{"xhigh": "high", "max": "high"} //nolint:exhaustive // Sparse aliases intentionally exclude canonical efforts and off values.
	case "deepseek-v4-pro-ga-260813", "deepseek-v4-flash-ga-260731":
		c.Efforts = []ReasoningEffort{"low", "high", "max"}
		c.Aliases = map[ReasoningEffort]ReasoningEffort{"medium": "low", "xhigh": "high"} //nolint:exhaustive // Sparse aliases intentionally exclude canonical efforts and off values.
	case "deepseek-v4-pro-260425", "deepseek-v4-flash-260425", "glm-5-2-260617":
		c.Efforts = []ReasoningEffort{"high", "max"}
		c.Aliases = map[ReasoningEffort]ReasoningEffort{"low": "high", "medium": "high", "xhigh": "max"} //nolint:exhaustive // Sparse aliases intentionally exclude canonical efforts and off values.
	default:
		return ReasoningContract{}, false
	}
	return c, true
}

// ValidateModelReasoningCapabilities validates a business declaration without
// filling defaults or changing its selectable subset. Unknown models retain
// manual declarations; they do not acquire inferred effort semantics.
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
	contract, known := LookupReasoningContract(provider, model)
	if known && caps.Reasoning == ReasoningSupportNone {
		return fmt.Errorf("loom: %s/%s has documented reasoning efforts; reasoning=none contradicts its contract", provider, model)
	}
	for _, effort := range caps.ReasoningEfforts {
		if !ValidReasoningEffort(effort) {
			return fmt.Errorf("loom: invalid declared reasoning effort %q", effort)
		}
		if seen[effort] {
			return fmt.Errorf("loom: duplicate declared reasoning effort %q", effort)
		}
		seen[effort] = true
		if known {
			if err := contract.ValidateEffort(effort); err != nil {
				return err
			}
		}
		if err := validateProviderEffort(provider, effort); err != nil {
			return err
		}
	}
	if caps.Reasoning == ReasoningSupportNone && len(caps.ReasoningEfforts) > 0 {
		return fmt.Errorf("loom: reasoning=none cannot declare reasoning efforts")
	}
	if known && caps.Reasoning != "" && caps.Reasoning != ReasoningSupportNone && len(caps.ReasoningEfforts) == 0 {
		return fmt.Errorf("loom: %s/%s requires an explicit reasoning effort declaration (canonical efforts: %v)", provider, model, contract.Efforts)
	}
	return nil
}

// ValidReasoningEffort checks only syntax, not a global effort vocabulary.
// Semantic validation belongs to the provider/model contract and capabilities.
func ValidReasoningEffort(effort ReasoningEffort) bool {
	if effort == "" {
		return false
	}
	for _, r := range effort {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func validateProviderEffort(provider string, effort ReasoningEffort) error {
	if provider == "ark" && (effort == "none" || effort == "minimal") || provider == "openrouter" && effort == "none" {
		return fmt.Errorf("loom: %s effort %q disables reasoning; explicitly select Mode=disabled with an empty effort", provider, effort)
	}
	var supported []ReasoningEffort
	switch provider {
	case "openrouter":
		supported = []ReasoningEffort{"minimal", "low", "medium", "high", "xhigh", "max"}
	case "deepseek":
		supported = []ReasoningEffort{"high", "max"}
	case "zhipuai":
		supported = []ReasoningEffort{"low", "medium", "high", "max"}
	}
	if supported != nil && !slices.Contains(supported, effort) {
		return fmt.Errorf("loom: %s adapter does not support reasoning effort %q (wire values: %v)", provider, effort, supported)
	}

	return nil
}

// ResolveModelReasoning applies provider/model semantics AND the caller's
// explicit capabilities. It never installs a contract as business configuration,
// normalizes an alias, picks an effort, or contacts a remote model catalog.
func ResolveModelReasoning(provider, model string, caps ModelCapabilities, r Reasoning) (ResolvedReasoning, error) {
	if !caps.reasoningProbe {
		if r.Effort != "" {
			if err := validateProviderEffort(provider, r.Effort); err != nil {
				return ResolvedReasoning{}, err
			}
			if c, ok := LookupReasoningContract(provider, model); ok {
				if err := c.ValidateEffort(r.Effort); err != nil {
					return ResolvedReasoning{}, err
				}
			}
		}
		if err := ValidateModelReasoningCapabilities(provider, model, caps); err != nil {
			return ResolvedReasoning{}, err
		}
		if r.Mode == ReasoningModeEnabled && r.Effort == "" {
			if c, ok := LookupReasoningContract(provider, model); ok {
				return ResolvedReasoning{}, fmt.Errorf("loom: %s/%s enabled reasoning requires an explicit effort (canonical efforts: %v)", provider, model, c.Efforts)
			}
		}
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
