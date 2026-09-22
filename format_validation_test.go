package loom

import (
	jsonv2 "encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
)

// TestFormatCases runs the specification's own cases for the four formats Loom's helpers cover.
//
// They live under optional/ in the suite because the specification makes format an annotation,
// asserted only by an implementation that opts into the format-assertion vocabulary. Loom
// follows the specification in ValidateSchema, which asserts no format, and asserts these one
// layer up in the contract, next to the shape pattern that already passed.
func TestFormatCases(t *testing.T) {
	t.Parallel()
	builders := map[string]func(string) *StringArg{
		"date":      Date,
		"time":      Time,
		"date-time": DateTime,
		"uuid":      UUID,
	}
	for format, build := range builders {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			var run, skipped int
			data, err := os.ReadFile(filepath.Join("testdata", "format", format+".json"))
			if err != nil {
				t.Fatalf("read fixtures: %v", err)
			}
			var groups []struct {
				Tests []struct {
					Description string `json:"description"`
					Data        any    `json:"data"`
					Valid       bool   `json:"valid"`
				} `json:"tests"`
			}
			if err := jsonv2.Unmarshal(data, &groups); err != nil {
				t.Fatalf("decode fixtures: %v", err)
			}
			for _, group := range groups {
				for _, testCase := range group.Tests {
					// A format does not constrain a non-string, which a contract says with
					// type: string before any format check runs.
					value, ok := testCase.Data.(string)
					if !ok {
						skipped++
						continue
					}
					run++
					field := build("f")
					contract := MustArgsContract("format_case", field)
					arguments, err := jsonv2.Marshal(map[string]any{"f": value})
					if err != nil {
						t.Fatalf("marshal case: %v", err)
					}
					_, decodeErr := contract.Decode(string(arguments))
					if got := decodeErr == nil; got != testCase.Valid {
						t.Errorf("%s: %q accepted=%v, want %v (%s)", format, value, got, testCase.Valid, testCase.Description)
					}
				}
			}
			t.Logf("format cases: run=%d skipped(non-string)=%d", run, skipped)
		})
	}
}
