package modelprobe

import (
	"context"
	"slices"
	"testing"

	"github.com/loomagent/loom"
)

func TestProbeSnapshotsOptionsBeforeBuildingModels(t *testing.T) {
	for _, test := range []struct {
		name             string
		candidates       []loom.ReasoningEffort
		wantCandidates   []loom.ReasoningEffort
		emptyDeclaration bool
	}{
		{"declared candidates", nil, []loom.ReasoningEffort{"high", "low"}, false},
		{"explicit candidates", []loom.ReasoningEffort{"low"}, []loom.ReasoningEffort{"low"}, false},
		{"explicitly no candidates", []loom.ReasoningEffort{}, []loom.ReasoningEffort{}, false},
		{"known empty effort universe", nil, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			declared := loom.ModelCapabilities{Reasoning: loom.ReasoningSupportToggleable, ReasoningEfforts: []loom.ReasoningEffort{"high", "low"}}
			var wantDeclared []loom.ReasoningEffort
			if test.emptyDeclaration {
				declared.ReasoningEfforts = nil
			} else {
				wantDeclared = []loom.ReasoningEffort{"high", "low"}
			}
			sources := []string{"https://example.invalid/original"}
			options := Options{DeclaredCapabilities: &declared, ReasoningEfforts: test.candidates, DeclarationSourceURLs: sources}
			firstBuild := true
			var sentEfforts []loom.ReasoningEffort
			builder := BuilderFunc(func(_ context.Context, caps loom.ModelCapabilities) (loom.ChatModel, error) {
				if firstBuild {
					firstBuild = false
					// A host may reload its configuration while constructing clients.
					if len(declared.ReasoningEfforts) > 0 {
						declared.ReasoningEfforts[0] = "max"
					}
					declared.Reasoning = ""
					sources[0] = "https://example.invalid/updated"
					if len(test.candidates) > 0 {
						test.candidates[0] = "xhigh"
					}
				}
				return &fakeModel{capabilities: caps, handler: func(_ loom.ModelCapabilities, request loom.ChatRequest) (*loom.ChatResponse, error) {
					if request.Reasoning.Effort != "" {
						sentEfforts = append(sentEfforts, request.Reasoning.Effort)
					}
					if request.StructuredOutput != nil || request.ResponseFormat == loom.ResponseFormatJSONObject {
						return &loom.ChatResponse{Content: validStructuredResponse(t, request), FinishReason: loom.FinishReasonStop}, nil
					}
					return reasoningResponse(0), nil
				}}, nil
			})
			report, err := Probe(t.Context(), builder, options)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(sentEfforts, test.wantCandidates) {
				t.Errorf("probed efforts = %v, want original candidates %v", sentEfforts, test.wantCandidates)
			}
			coverage := report.EffortCoverage
			if !slices.Equal(coverage.Declared, wantDeclared) || !coverage.UniverseKnown {
				t.Errorf("report changed its original declaration: %+v", coverage)
			}
			if !slices.Equal(coverage.SourceURLs, []string{"https://example.invalid/original"}) {
				t.Errorf("report changed its original sources: %v", coverage.SourceURLs)
			}
		})
	}
}
