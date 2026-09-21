package loom

import (
	"strings"
	"testing"
)

// TestResolveReasoningMatrix covers the whole ResolveReasoning matrix: every
// combination of capability and request.
func TestResolveReasoningMatrix(t *testing.T) {
	tests := []struct {
		name    string
		caps    ReasoningSupport
		mode    ReasoningMode
		effort  ReasoningEffort
		want    ReasoningSend
		wantErr string // non-empty = an error whose message contains this substring
	}{
		// ===== Mode is required: every zero value fails =====
		{name: "undeclared Mode x none", caps: ReasoningSupportNone, mode: "", wantErr: "is required"},
		{name: "undeclared Mode x always_on", caps: ReasoningSupportAlwaysOn, mode: "", wantErr: "is required"},
		{name: "undeclared Mode x toggleable_on", caps: ReasoningSupportToggleableDefaultOn, mode: "", wantErr: "is required"},
		{name: "undeclared Mode x undeclared capability", caps: "", mode: "", wantErr: "is required"},

		// ===== the Enabled row =====
		{name: "Enabled x none", caps: ReasoningSupportNone, mode: ReasoningModeEnabled, wantErr: "supports no reasoning"},
		{name: "Enabled x always_on", caps: ReasoningSupportAlwaysOn, mode: ReasoningModeEnabled, want: ReasoningSendEnabled},
		{name: "Enabled x toggleable_on", caps: ReasoningSupportToggleableDefaultOn, mode: ReasoningModeEnabled, want: ReasoningSendEnabled},
		{name: "Enabled x toggleable_off", caps: ReasoningSupportToggleableDefaultOff, mode: ReasoningModeEnabled, want: ReasoningSendEnabled},
		{name: "Enabled x undeclared capability passes through", caps: "", mode: ReasoningModeEnabled, want: ReasoningSendEnabled},

		// ===== the Disabled row =====
		{name: "Disabled x none sends nothing", caps: ReasoningSupportNone, mode: ReasoningModeDisabled, want: ReasoningSendOmit},
		{name: "Disabled x always_on", caps: ReasoningSupportAlwaysOn, mode: ReasoningModeDisabled, wantErr: "cannot turn reasoning off"},
		{name: "Disabled x toggleable_on", caps: ReasoningSupportToggleableDefaultOn, mode: ReasoningModeDisabled, want: ReasoningSendDisabled},
		{name: "Disabled x toggleable_off", caps: ReasoningSupportToggleableDefaultOff, mode: ReasoningModeDisabled, want: ReasoningSendDisabled},
		{name: "Disabled x undeclared capability passes through", caps: "", mode: ReasoningModeDisabled, want: ReasoningSendDisabled},

		// ===== invalid values =====
		{name: "unknown Mode", caps: "", mode: "auto", wantErr: "unknown Reasoning.Mode"},
		{name: "unknown Effort", caps: "", mode: ReasoningModeEnabled, effort: "bad effort", wantErr: "invalid Reasoning.Effort"},

		// ===== the Effort cross-check =====
		{name: "Disabled with an Effort contradicts itself", caps: ReasoningSupportToggleableDefaultOn, mode: ReasoningModeDisabled, effort: ReasoningEffortHigh, wantErr: "contradicts"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveReasoning(
				ModelCapabilities{Reasoning: tt.caps},
				Reasoning{Mode: tt.mode, Effort: tt.effort},
			)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got %+v", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if got.Send != tt.want {
				t.Fatalf("Send = %q, want %q", got.Send, tt.want)
			}
		})
	}
}

// TestResolveReasoningEfforts covers the capability check on reasoning efforts.
func TestResolveReasoningEfforts(t *testing.T) {
	capsWithEfforts := ModelCapabilities{
		Reasoning:        ReasoningSupportToggleableDefaultOn,
		ReasoningEfforts: []ReasoningEffort{ReasoningEffortHigh, ReasoningEffortMax},
	}
	capsNoEfforts := ModelCapabilities{
		Reasoning: ReasoningSupportToggleableDefaultOn,
	}

	t.Run("an effort in the declared list", func(t *testing.T) {
		got, err := ResolveReasoning(capsWithEfforts, Reasoning{Mode: ReasoningModeEnabled, Effort: ReasoningEffortHigh})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if got.Effort != ReasoningEffortHigh {
			t.Fatalf("Effort = %q, want high", got.Effort)
		}
	})

	t.Run("an effort outside the list fails", func(t *testing.T) {
		onlyHigh := ModelCapabilities{
			Reasoning:        ReasoningSupportToggleableDefaultOn,
			ReasoningEfforts: []ReasoningEffort{ReasoningEffortHigh},
		}
		_, err := ResolveReasoning(onlyHigh, Reasoning{Mode: ReasoningModeEnabled, Effort: ReasoningEffortMax})
		if err == nil || !strings.Contains(err.Error(), "does not support reasoning effort") {
			t.Fatalf("expected an out-of-range effort to fail, got %v", err)
		}
	})

	t.Run("doubao accepts low and medium", func(t *testing.T) {
		doubao := ModelCapabilities{
			Reasoning:        ReasoningSupportToggleableDefaultOn,
			ReasoningEfforts: []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
		}
		for _, effort := range []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium} {
			got, err := ResolveReasoning(doubao, Reasoning{Mode: ReasoningModeEnabled, Effort: effort})
			if err != nil {
				t.Fatalf("effort=%s expected success, got %v", effort, err)
			}
			if got.Effort != effort {
				t.Fatalf("Effort = %q, want %q", got.Effort, effort)
			}
		}
		// max belongs to deepseek; it is outside doubao's declared list
		if _, err := ResolveReasoning(doubao, Reasoning{Mode: ReasoningModeEnabled, Effort: ReasoningEffortMax}); err == nil {
			t.Fatal("expected max to be out of range")
		}
	})

	t.Run("a declared model without efforts rejects an invented one", func(t *testing.T) {
		if _, err := ResolveReasoning(capsNoEfforts, Reasoning{Mode: ReasoningModeEnabled, Effort: ReasoningEffortMax}); err == nil {
			t.Fatal("must reject effort on a model declared without effort selection")
		}
	})

	t.Run("declared efforts must be selected explicitly", func(t *testing.T) {
		if _, err := ResolveReasoning(capsWithEfforts, Reasoning{Mode: ReasoningModeEnabled}); err == nil {
			t.Fatal("missing effort must fail")
		}
	})
	t.Run("an undeclared probe capability may omit the effort", func(t *testing.T) {
		if _, err := ResolveReasoning(ModelCapabilities{}, Reasoning{Mode: ReasoningModeEnabled}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestToggleableCanonicalCompatibility(t *testing.T) {
	for _, support := range []ReasoningSupport{ReasoningSupportToggleable, ReasoningSupportToggleableDefaultOn, ReasoningSupportToggleableDefaultOff} {
		if support.Canonical() != ReasoningSupportToggleable {
			t.Fatalf("canonical(%q)", support)
		}
		caps := ModelCapabilities{Reasoning: support, ReasoningEfforts: []ReasoningEffort{ReasoningEffortLow}}
		if _, err := ResolveReasoning(caps, Reasoning{Mode: ReasoningModeEnabled}); err == nil {
			t.Fatal("missing effort accepted")
		}
		if _, err := ResolveReasoning(caps, Reasoning{Mode: ReasoningModeEnabled, Effort: ReasoningEffortLow}); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveReasoning(caps, Reasoning{Mode: ReasoningModeDisabled}); err != nil {
			t.Fatal(err)
		}
	}
}
