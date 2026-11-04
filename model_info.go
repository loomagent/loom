package loom

// ModelRef is an opaque host-owned reference. Loom neither allocates nor resolves
// it. An empty field is unknown; Name, URLs and credentials are never used to
// manufacture an instance or snapshot identity.
type ModelRef struct {
	InstanceID string `json:"instance_id,omitempty"`
	SnapshotID string `json:"snapshot_id,omitempty"`
}

// ModelInfo describes the configured request target, not a provider's response.
// Capabilities remain available exclusively through ChatModel.Capabilities.
type ModelInfo struct {
	Ref          ModelRef `json:"ref,omitzero"`
	Provider     string   `json:"provider,omitempty"`
	RequestModel string   `json:"request_model,omitempty"`
}

// ModelInfoProvider is optional; existing custom ChatModels need no new methods.
// A wrapper must forward this interface explicitly to preserve a binding.
type ModelInfoProvider interface {
	ModelInfo() ModelInfo
}

// DescribeModel reads a value snapshot. Unknown models stay unbound, even when
// their display Name resembles a built-in provider's name.
func DescribeModel(model ChatModel) ModelInfo {
	if provider, ok := model.(ModelInfoProvider); ok {
		return provider.ModelInfo()
	}
	return ModelInfo{}
}
