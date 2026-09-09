package loom

import (
	"slices"
	"strings"
	"testing"
)

func TestReasoningContracts(t *testing.T) {
	for _, tc := range []struct {
		model   string
		efforts []ReasoningEffort
		aliases map[ReasoningEffort]ReasoningEffort
	}{
		{"doubao-seed-evolving", []ReasoningEffort{"low", "medium", "high"}, map[ReasoningEffort]ReasoningEffort{"max": "high", "xhigh": "high"}}, //nolint:exhaustive // Sparse aliases intentionally exclude canonical efforts and off values.
		{"doubao-seed-2-1-pro-260628", []ReasoningEffort{"low", "medium", "high"}, map[ReasoningEffort]ReasoningEffort{"max": "high"}},            //nolint:exhaustive // Sparse aliases intentionally exclude canonical efforts and off values.
		{"doubao-seed-2-1-turbo-260628", []ReasoningEffort{"low", "medium", "high"}, nil},
		{"deepseek-v4-pro-ga-260813", []ReasoningEffort{"low", "high", "max"}, map[ReasoningEffort]ReasoningEffort{"medium": "low", "xhigh": "high"}}, //nolint:exhaustive // Sparse aliases intentionally exclude canonical efforts and off values.
		{"deepseek-v4-flash-ga-260731", []ReasoningEffort{"low", "high", "max"}, nil},
		{"deepseek-v4-pro-260425", []ReasoningEffort{"high", "max"}, map[ReasoningEffort]ReasoningEffort{"medium": "high", "xhigh": "max"}}, //nolint:exhaustive // Sparse aliases intentionally exclude canonical efforts and off values.
		{"deepseek-v4-flash-260425", []ReasoningEffort{"high", "max"}, nil},
		{"glm-5-2-260617", []ReasoningEffort{"high", "max"}, nil},
	} {
		t.Run(tc.model, func(t *testing.T) {
			c, ok := LookupReasoningContract("ark", tc.model)
			if !ok || !slices.Equal(c.Efforts, tc.efforts) || c.Source == "" || len(c.SourceURLs) == 0 || c.Match == "" {
				t.Fatalf("contract=%+v", c)
			}
			caps := ModelCapabilities{Reasoning: ReasoningSupportToggleable, ReasoningEfforts: c.Efforts}
			for _, e := range c.Efforts {
				r, err := ResolveModelReasoning("ark", tc.model, caps, Reasoning{Mode: ReasoningModeEnabled, Effort: e})
				if err != nil || r.Effort != e {
					t.Fatalf("%s: %+v %v", e, r, err)
				}
			}
			for e, target := range tc.aliases {
				if c.Aliases[e] != target {
					t.Fatalf("alias=%s", c.Aliases[e])
				}
				_, err := ResolveModelReasoning("ark", tc.model, caps, Reasoning{Mode: ReasoningModeEnabled, Effort: e})
				if err == nil || !strings.Contains(err.Error(), "alias of \""+string(target)+"\"") {
					t.Fatalf("alias error=%v", err)
				}
			}
			for _, e := range c.OffAliases {
				_, err := ResolveModelReasoning("ark", tc.model, caps, Reasoning{Mode: ReasoningModeEnabled, Effort: e})
				if err == nil || !strings.Contains(err.Error(), "disables reasoning") {
					t.Fatalf("off error=%v", err)
				}
			}
			if _, err := ResolveModelReasoning("ark", tc.model, caps, Reasoning{Mode: ReasoningModeEnabled}); err == nil {
				t.Fatal("missing effort accepted")
			}
			if _, err := ResolveModelReasoning("ark", tc.model, caps, Reasoning{Mode: ReasoningModeDisabled}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReasoningContractsDoNotGuessOrLeakMutableState(t *testing.T) {
	for _, model := range []string{"doubao-seed-evolving-latest-version", "doubao-seed-2-1-pro-260629", "doubao-seed-2-1-turbo", "glm-5-2", "deepseek-v4-pro-260813", "deepseek-v4-flash-260731", "deepseek-v4-pro-preview-260425", "deepseek-v4-flash-preview-260425", "ep-private", "deepseek-v4-pro", "deepseek-v4-pro-260814", "deepseek-v4-pro-preview-260426", "glm-5-3", "doubao-seed-2-1-pro-custom", "doubao-seed-2-2-pro", "doubao-seed-evolving-custom"} {
		if _, ok := LookupReasoningContract("ark", model); ok {
			t.Fatalf("guessed contract for %s", model)
		}
	}
	if _, ok := LookupReasoningContract("openrouter", "deepseek-v4-pro-ga-260813"); ok {
		t.Fatal("provider boundary ignored")
	}
	c, _ := LookupReasoningContract("ark", "doubao-seed-evolving")
	c.Efforts[0] = "changed"
	c.Aliases["max"] = "changed"
	c, _ = LookupReasoningContract("ark", "doubao-seed-evolving")
	if c.Efforts[0] != "low" || c.Aliases["max"] != "high" {
		t.Fatal("mutable global state leaked")
	}
}

func TestReasoningExplicitDeclarationsAndProbeIsolation(t *testing.T) {
	for _, effort := range []ReasoningEffort{"minimal", "xhigh", "max"} {
		caps := ModelCapabilities{Reasoning: ReasoningSupportAlwaysOn, ReasoningEfforts: []ReasoningEffort{effort}}
		got, err := ResolveModelReasoning("openrouter", "declared-model", caps, Reasoning{Mode: ReasoningModeEnabled, Effort: effort})
		if err != nil || got.Send != ReasoningSendEnabled || got.Effort != effort {
			t.Fatalf("%s: %+v %v", effort, got, err)
		}
	}
	noEfforts := ModelCapabilities{Reasoning: ReasoningSupportToggleable}
	if _, err := ResolveReasoning(noEfforts, Reasoning{Mode: ReasoningModeEnabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveReasoning(noEfforts, Reasoning{Mode: ReasoningModeEnabled, Effort: "high"}); err == nil {
		t.Fatal("invented effort on no-effort model")
	}
	c, _ := LookupReasoningContract("ark", "deepseek-v4-pro-ga-260813")
	bad := ModelCapabilities{Reasoning: ReasoningSupportToggleable, ReasoningEfforts: []ReasoningEffort{"medium"}}
	if err := ValidateModelReasoningCapabilities(c.Provider, c.Model, bad); err == nil {
		t.Fatal("alias declaration accepted")
	}
	good := ModelCapabilities{Reasoning: ReasoningSupportToggleable, ReasoningEfforts: []ReasoningEffort{"max"}}
	if _, err := ResolveModelReasoning(c.Provider, c.Model, good, Reasoning{Mode: ReasoningModeEnabled, Effort: "low"}); err == nil {
		t.Fatal("contract overrode explicit subset")
	}
	for _, effort := range []ReasoningEffort{"", "medium", "minimal", "xhigh"} {
		_, err := ResolveModelReasoning(c.Provider, c.Model, ReasoningProbeCapabilities(false), Reasoning{Mode: ReasoningModeEnabled, Effort: effort})
		if err != nil {
			t.Fatalf("isolated probe %q: %v", effort, err)
		}
	}
	if _, err := ResolveModelReasoning(c.Provider, c.Model, ModelCapabilities{}, Reasoning{Mode: ReasoningModeEnabled, Effort: "medium"}); err == nil {
		t.Fatal("alias allowed outside probe")
	}
	if _, err := ResolveModelReasoning(c.Provider, c.Model, ModelCapabilities{}, Reasoning{Mode: ReasoningModeEnabled}); err == nil {
		t.Fatal("missing effort on known model outside probe")
	}
}

func TestNativeEffortVocabularyIsOpenButAdapterSupportIsExplicit(t *testing.T) {
	caps := ModelCapabilities{Reasoning: ReasoningSupportToggleable, ReasoningEfforts: []ReasoningEffort{"vendor-native-7"}}
	if _, err := ResolveReasoning(caps, Reasoning{Mode: ReasoningModeEnabled, Effort: "vendor-native-7"}); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"openrouter", "deepseek", "zhipuai"} {
		if err := ValidateModelReasoningCapabilities(provider, "unknown", caps); err == nil {
			t.Fatalf("%s saved effort its adapter cannot send", provider)
		}
	}
}

func TestOlderExactSeedContracts(t *testing.T) {
	for _, model := range []string{
		"doubao-seed-2-0-lite-260428", "doubao-seed-2-0-mini-260428",
		"doubao-seed-2-0-pro-260215", "doubao-seed-2-0-lite-260215", "doubao-seed-2-0-mini-260215", "doubao-seed-2-0-code-preview-260215",
		"doubao-seed-1-8-251228", "doubao-seed-1-6-251015", "doubao-seed-character-260628",
	} {
		c, ok := LookupReasoningContract("ark", model)
		if !ok || !slices.Equal(c.Efforts, []ReasoningEffort{"low", "medium", "high"}) || c.Aliases["max"] != "high" || c.Match != model {
			t.Fatalf("%s: %+v", model, c)
		}
	}
}

func TestKnownReasoningCannotBeDeclaredNone(t *testing.T) {
	caps := ModelCapabilities{Reasoning: ReasoningSupportNone}
	for _, model := range []string{"doubao-seed-evolving", "deepseek-v4-pro-ga-260813", "deepseek-v4-flash-260425", "glm-5-2-260617"} {
		if err := ValidateModelReasoningCapabilities("ark", model, caps); err == nil {
			t.Fatalf("%s saved as none", model)
		}
		if _, err := ResolveModelReasoning("ark", model, caps, Reasoning{Mode: ReasoningModeDisabled}); err == nil {
			t.Fatalf("%s omitted disabled control outside a probe", model)
		}
	}
}
