package loom

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestExplainSchemaErrorUsesSchemaNotCauseText(t *testing.T) {
	type item struct {
		Name string `json:"name" validate:"min=2"`
	}
	type request struct {
		Mode  string         `json:"mode" validate:"oneof=fast deep"`
		Query string         `json:"query" validate:"min=2"`
		Limit int            `json:"limit,omitempty" validate:"omitempty,min=0,max=5"`
		Items []item         `json:"items,omitempty" validate:"omitempty,min=2"`
		Meta  map[string]any `json:"meta,omitempty" validate:"omitempty,max=1"`
	}

	schema := MustSchemaFor[request]()
	var instance any
	if err := json.Unmarshal([]byte(`{"query":"","limit":6,"items":[{"name":"x"}],"meta":{"a":1,"b":2},"extra":true}`), &instance); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(instance); err == nil {
		t.Fatal("test instance unexpectedly satisfies schema")
	}

	// The cause intentionally contains no jsonschema-go wording. Diagnostics
	// must be derived from schema and instance only.
	issues := explainSchemaError(schema, instance, errors.New("upstream error text changed completely"))
	got := make(map[string]string, len(issues))
	for _, issue := range issues {
		got[issue.Field+":"+issue.Rule] = issue.Message
	}
	for key, want := range map[string]string{
		"mode:required":     `"mode" is required`,
		"query:min":         `"query" must contain at least 2 characters`,
		"limit:max":         `"limit" must be at most 5`,
		"items:min":         `"items" must contain at least 2 items`,
		"items[0].name:min": `"items[0].name" must contain at least 2 characters`,
		"meta:max":          `"meta" must contain at most 1 fields`,
		"extra:unknown":     `"extra" is not an accepted field`,
	} {
		if got[key] != want {
			t.Errorf("issue %s = %q, want %q; all=%v", key, got[key], want, got)
		}
	}
}

func TestExplainSchemaErrorFallsBackForUnsupportedKeywords(t *testing.T) {
	// Two empty branches make oneOf fail because both match. oneOf is not part
	// of Loom's generated validator projection, so the generic fallback applies.
	schema := &jsonschema.Schema{OneOf: []*jsonschema.Schema{{}, {}}}
	issues := explainSchemaError(schema, "c", errors.New("opaque"))
	if len(issues) != 1 || issues[0].Rule != "schema" || !strings.Contains(issues[0].Message, "expected schema") {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestToolArgumentErrorBoundsModelFacingOutput(t *testing.T) {
	issues := make([]ToolArgumentIssue, 12)
	for index := range issues {
		issues[index] = ToolArgumentIssue{Message: strings.Repeat("问题", maxToolArgumentMessageRunes)}
	}
	err := (&ToolArgumentError{
		Issues:            issues,
		ExpectedArguments: strings.Repeat("参", maxExpectedArgumentRunes+10),
		ExampleArguments:  strings.Repeat("例", maxExampleArgumentRunes+1),
	}).Error()
	if !strings.Contains(err, "and 4 more validation issues") {
		t.Fatalf("missing issue summary: %s", err)
	}
	if !strings.Contains(err, "example arguments omitted") {
		t.Fatalf("oversized example was not omitted: %s", err)
	}
	if len([]rune(err)) > maxToolArgumentMessages*(maxToolArgumentMessageRunes+2)+maxExpectedArgumentRunes+300 {
		t.Fatalf("model-facing error is unexpectedly large: %d runes", len([]rune(err)))
	}
}
