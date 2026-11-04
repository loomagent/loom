package modelprobe

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/loomagent/loom"
)

type boundProbeModel struct {
	loom.ChatModel
	ref loom.ModelRef
}

func (m *boundProbeModel) ModelInfo() loom.ModelInfo {
	return loom.ModelInfo{Ref: m.ref, Provider: "test", RequestModel: "same-alias"}
}

func TestProbeCapturesTargetAndMeasurementInterval(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "partial"}[partial], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			target := loom.ModelRef{InstanceID: "provider:alias", SnapshotID: "snapshot-before-build"}
			want := target
			started := time.Now()
			builder := BuilderFunc(func(_ context.Context, caps loom.ModelCapabilities) (loom.ChatModel, error) {
				target.SnapshotID = "reloaded-during-build"
				return &boundProbeModel{ref: want, ChatModel: &fakeModel{capabilities: caps, handler: func(_ loom.ModelCapabilities, request loom.ChatRequest) (*loom.ChatResponse, error) {
					if partial {
						cancel()
					}
					if request.StructuredOutput != nil || request.ResponseFormat == loom.ResponseFormatJSONObject {
						return &loom.ChatResponse{Content: validStructuredResponse(t, request), FinishReason: loom.FinishReasonStop}, nil
					}
					return reasoningResponse(0), nil
				}}}, nil
			})
			report, err := Probe(ctx, builder, Options{TargetRef: &target})
			if partial {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("partial error changed: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if report.TargetRef == nil || *report.TargetRef != want {
				t.Fatalf("target mutated: %+v", report.TargetRef)
			}
			if report.StartedAt.Before(started) || report.FinishedAt.Before(report.StartedAt) || report.FinishedAt.After(time.Now()) {
				t.Fatal("measurement interval missing or invalid")
			}
			wire, err := jsonv2.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var roundtrip Report
			if err := jsonv2.Unmarshal(wire, &roundtrip); err != nil {
				t.Fatal(err)
			}
			if roundtrip.TargetRef == nil || *roundtrip.TargetRef != want || !roundtrip.FinishedAt.Equal(report.FinishedAt) {
				t.Fatal("report lost identity or interval on JSON roundtrip")
			}
			t.Logf("target=%+v checks=%d started=%s finished=%s", *report.TargetRef, len(report.Checks), report.StartedAt, report.FinishedAt)
		})
	}
}

func TestProbeRejectsConflictingBindingsBeforeRequests(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "different providers same alias", true: "different explicit target"}[explicit], func(t *testing.T) {
			builds, calls := 0, 0
			builder := BuilderFunc(func(_ context.Context, caps loom.ModelCapabilities) (loom.ChatModel, error) {
				builds++
				ref := loom.ModelRef{InstanceID: "account-A:alias", SnapshotID: "snapshot-A"}
				if builds == 2 && !explicit {
					ref = loom.ModelRef{InstanceID: "account-B:alias", SnapshotID: "snapshot-B"}
				}
				return &boundProbeModel{ref: ref, ChatModel: &fakeModel{name: "same/alias", capabilities: caps, handler: func(loom.ModelCapabilities, loom.ChatRequest) (*loom.ChatResponse, error) {
					calls++
					return reasoningResponse(0), nil
				}}}, nil
			})
			opts := Options{}
			if explicit {
				opts.TargetRef = &loom.ModelRef{InstanceID: "account-C:alias", SnapshotID: "snapshot-C"}
			}
			_, err := Probe(t.Context(), builder, opts)
			if err == nil || calls != 0 {
				t.Fatalf("conflicting binding reached provider: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestHistoricalProbeReportHasNoInferredTargetOrTimes(t *testing.T) {
	var report Report
	if err := jsonv2.Unmarshal([]byte(`{"schema_version":2,"model":"deepseek/alias","checks":[]}`), &report); err != nil {
		t.Fatal(err)
	}
	if report.TargetRef != nil || !report.StartedAt.IsZero() || !report.FinishedAt.IsZero() {
		t.Fatal("historical unknown binding or measurement time filled")
	}
	wire, err := jsonv2.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := jsonv2.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"target_ref", "started_at", "finished_at"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("unknown historical field present: %s", key)
		}
	}
}
