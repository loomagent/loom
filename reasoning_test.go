package loom

import (
	"strings"
	"testing"
)

// TestResolveReasoningMatrix 全覆盖 ResolveReasoning 的解析矩阵(能力×声明的全组合)。
func TestResolveReasoningMatrix(t *testing.T) {
	tests := []struct {
		name    string
		caps    ReasoningSupport
		mode    ReasoningMode
		effort  ReasoningEffort
		want    ReasoningSend
		wantErr string // 非空 = 期望报错且错误信息包含该子串
	}{
		// ===== Mode 必传(零值全列报错) =====
		{name: "未声明Mode×none", caps: ReasoningSupportNone, mode: "", wantErr: "必传"},
		{name: "未声明Mode×always_on", caps: ReasoningSupportAlwaysOn, mode: "", wantErr: "必传"},
		{name: "未声明Mode×toggleable_on", caps: ReasoningSupportToggleableDefaultOn, mode: "", wantErr: "必传"},
		{name: "未声明Mode×能力未声明", caps: "", mode: "", wantErr: "必传"},

		// ===== Enabled 行 =====
		{name: "Enabled×none", caps: ReasoningSupportNone, mode: ReasoningModeEnabled, wantErr: "不支持推理"},
		{name: "Enabled×always_on", caps: ReasoningSupportAlwaysOn, mode: ReasoningModeEnabled, want: ReasoningSendEnabled},
		{name: "Enabled×toggleable_on", caps: ReasoningSupportToggleableDefaultOn, mode: ReasoningModeEnabled, want: ReasoningSendEnabled},
		{name: "Enabled×toggleable_off", caps: ReasoningSupportToggleableDefaultOff, mode: ReasoningModeEnabled, want: ReasoningSendEnabled},
		{name: "Enabled×能力未声明透传", caps: "", mode: ReasoningModeEnabled, want: ReasoningSendEnabled},

		// ===== Disabled 行 =====
		{name: "Disabled×none不发参数", caps: ReasoningSupportNone, mode: ReasoningModeDisabled, want: ReasoningSendOmit},
		{name: "Disabled×always_on", caps: ReasoningSupportAlwaysOn, mode: ReasoningModeDisabled, wantErr: "不可关闭"},
		{name: "Disabled×toggleable_on", caps: ReasoningSupportToggleableDefaultOn, mode: ReasoningModeDisabled, want: ReasoningSendDisabled},
		{name: "Disabled×toggleable_off", caps: ReasoningSupportToggleableDefaultOff, mode: ReasoningModeDisabled, want: ReasoningSendDisabled},
		{name: "Disabled×能力未声明透传", caps: "", mode: ReasoningModeDisabled, want: ReasoningSendDisabled},

		// ===== 非法值 =====
		{name: "未知Mode", caps: "", mode: "auto", wantErr: "未知 Reasoning.Mode"},
		{name: "未知Effort", caps: "", mode: ReasoningModeEnabled, effort: "ultra", wantErr: "未知 Reasoning.Effort"},

		// ===== Effort 交叉校验 =====
		{name: "Disabled带Effort矛盾", caps: ReasoningSupportToggleableDefaultOn, mode: ReasoningModeDisabled, effort: ReasoningEffortHigh, wantErr: "矛盾"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveReasoning(
				ModelCapabilities{Reasoning: tt.caps},
				Reasoning{Mode: tt.mode, Effort: tt.effort},
			)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("期望报错(含 %q),实际 nil,got=%+v", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("错误信息 %q 不含 %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功,实际报错: %v", err)
			}
			if got.Send != tt.want {
				t.Fatalf("Send = %q, want %q", got.Send, tt.want)
			}
		})
	}
}

// TestResolveReasoningEfforts 推理强度档位的能力校验。
func TestResolveReasoningEfforts(t *testing.T) {
	capsWithEfforts := ModelCapabilities{
		Reasoning:        ReasoningSupportToggleableDefaultOn,
		ReasoningEfforts: []ReasoningEffort{ReasoningEffortHigh, ReasoningEffortMax},
	}
	capsNoEfforts := ModelCapabilities{
		Reasoning: ReasoningSupportToggleableDefaultOn,
	}

	t.Run("档位在能力清单内", func(t *testing.T) {
		got, err := ResolveReasoning(capsWithEfforts, Reasoning{Mode: ReasoningModeEnabled, Effort: ReasoningEffortHigh})
		if err != nil {
			t.Fatalf("期望成功,实际报错: %v", err)
		}
		if got.Effort != ReasoningEffortHigh {
			t.Fatalf("Effort = %q, want high", got.Effort)
		}
	})

	t.Run("档位越界报错", func(t *testing.T) {
		onlyHigh := ModelCapabilities{
			Reasoning:        ReasoningSupportToggleableDefaultOn,
			ReasoningEfforts: []ReasoningEffort{ReasoningEffortHigh},
		}
		_, err := ResolveReasoning(onlyHigh, Reasoning{Mode: ReasoningModeEnabled, Effort: ReasoningEffortMax})
		if err == nil || !strings.Contains(err.Error(), "不支持推理强度档位") {
			t.Fatalf("期望档位越界报错,实际: %v", err)
		}
	})

	t.Run("doubao档位low/medium合法", func(t *testing.T) {
		doubao := ModelCapabilities{
			Reasoning:        ReasoningSupportToggleableDefaultOn,
			ReasoningEfforts: []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh},
		}
		for _, effort := range []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium} {
			got, err := ResolveReasoning(doubao, Reasoning{Mode: ReasoningModeEnabled, Effort: effort})
			if err != nil {
				t.Fatalf("effort=%s 期望成功,实际报错: %v", effort, err)
			}
			if got.Effort != effort {
				t.Fatalf("Effort = %q, want %q", got.Effort, effort)
			}
		}
		// max 是 deepseek 专属档位,doubao 能力清单外 → 报错
		if _, err := ResolveReasoning(doubao, Reasoning{Mode: ReasoningModeEnabled, Effort: ReasoningEffortMax}); err == nil {
			t.Fatal("期望 max 越界报错,实际成功")
		}
	})

	t.Run("能力未声明档位时透传", func(t *testing.T) {
		got, err := ResolveReasoning(capsNoEfforts, Reasoning{Mode: ReasoningModeEnabled, Effort: ReasoningEffortMax})
		if err != nil {
			t.Fatalf("期望透传成功,实际报错: %v", err)
		}
		if got.Effort != ReasoningEffortMax {
			t.Fatalf("Effort = %q, want max", got.Effort)
		}
	})

	t.Run("不带Effort合法", func(t *testing.T) {
		got, err := ResolveReasoning(capsWithEfforts, Reasoning{Mode: ReasoningModeEnabled})
		if err != nil {
			t.Fatalf("期望成功,实际报错: %v", err)
		}
		if got.Effort != ReasoningEffortDefault {
			t.Fatalf("Effort = %q, want 空", got.Effort)
		}
	})
}
