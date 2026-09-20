package toolcontract

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/loomagent/loom/internal/schema"
)

// TestJSONSchemaSuiteSubset runs the official JSON Schema Test Suite over the
// keywords Loom implements. The suite covers far more than Loom supports, so a
// case whose schema does not decode into Loom's model — a $ref, a composition,
// a type union, schema-valued additionalProperties, tuple items — is skipped
// rather than approximated. That is what keeps this a check on Loom's own subset
// instead of pressure to grow it.
//
// A skipped case is not asserted to be unsupported forever; the numbers are
// logged so the supported surface is visible when it changes.
func TestJSONSchemaSuiteSubset(t *testing.T) {
	var decoded, skippedDecode, skippedCompile, cases, deviations int
	hit := make(map[string]bool, len(knownDeviations))

	for _, file := range suiteFiles(t) {
		for _, group := range readSuiteFile(t, file) {
			var s schema.Schema
			if err := jsonv2.Unmarshal(group.Schema, &s); err != nil {
				skippedDecode++
				continue
			}
			validator, err := Compile(&s)
			if err != nil {
				// The model refuses to decode keywords it cannot enforce, so a
				// schema that decodes must compile. A failure here means the two
				// disagree.
				skippedCompile++
				t.Errorf("%s: decodes but does not compile: %v", file, err)
				continue
			}
			decoded++
			for _, tc := range group.Tests {
				cases++
				got := validator.Validate(tc.Data) == nil
				if got == tc.Valid {
					continue
				}
				key := filepath.Base(file) + "|" + group.Description + "|" + tc.Description
				reason, known := knownDeviations[key]
				if !known {
					t.Errorf("unexpected disagreement: %s: got valid=%v want %v (schema=%s data=%s)",
						key, got, tc.Valid, group.Schema, tc.Data)
					continue
				}
				hit[key] = true
				deviations++
				t.Logf("known deviation — %s: %s", reason, key)
			}
		}
	}

	for key := range knownDeviations {
		if !hit[key] {
			t.Errorf("stale known deviation, no longer triggered: %s", key)
		}
	}
	if decoded == 0 || cases == 0 {
		t.Fatal("no suite case ran; the model or the testdata is wrong")
	}
	t.Logf("files=%d decodedGroups=%d skippedDecode=%d skippedCompile=%d cases=%d knownDeviations=%d",
		len(suiteFiles(t)), decoded, skippedDecode, skippedCompile, cases, deviations)
}

// knownDeviations records where Loom's deliberately narrower subset disagrees
// with the specification. Keyed by "file|group|case" and asserted to be hit, so
// a deviation that is fixed fails the test until its entry is removed.
var knownDeviations = map[string]string{
	"type.json|integer type matches integers|a float with zero fractional part is an integer": "Loom reads an integer as a number written without a fraction or exponent, so 1.0 must be sent as 1",
}

type suiteGroup struct {
	Description string         `json:"description"`
	Schema      jsontext.Value `json:"schema"`
	Tests       []suiteCase    `json:"tests"`
}

type suiteCase struct {
	Description string         `json:"description"`
	Data        jsontext.Value `json:"data"`
	Valid       bool           `json:"valid"`
}

func suiteFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "jsonschema", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func readSuiteFile(t *testing.T, file string) []suiteGroup {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var groups []suiteGroup
	if err := jsonv2.Unmarshal(raw, &groups); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return groups
}
