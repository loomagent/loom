package loom

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
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
	if err := jsonv2.Unmarshal([]byte(`{"query":"","limit":6,"items":[{"name":"x"}],"meta":{"a":1,"b":2},"extra":true}`), &instance); err != nil {
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
		"items[0].name:min": `"items[0].name" must contain at least 2 characters`,
		"meta:max":          `"meta" must contain at most 1 fields`,
		"extra:unknown":     `"extra" is not an accepted field`,
	} {
		if got[key] != want {
			t.Errorf("issue %s = %q, want %q; all=%v", key, got[key], want, got)
		}
	}

	if message, exists := got["items:min"]; exists {
		t.Errorf("items:min = %q, want no schema-level bound for an omitempty field", message)
	}
}

func TestExplainSchemaErrorFallsBackForUnsupportedKeywords(t *testing.T) {
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

// A value whose JSON type does not fit the declared argument must be reported as
// a schema problem that names the field, not as malformed JSON, and it must not
// leak a Go type name into the model-facing message.
func TestArgsTypeMismatchNamesFieldAndType(t *testing.T) {
	contract := MustArgsContract("web_search",
		String("query").Required().Desc("Search query."),
		Int("top_k").Desc("Result count."),
	)
	_, err := contract.Decode(`{"query":"go","top_k":"5"}`)
	if err == nil {
		t.Fatal("type mismatch must fail")
	}
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatalf("error type = %T", err)
	}
	if argumentError.Kind != ToolArgumentErrorSchema {
		t.Errorf("Kind = %q, want %q", argumentError.Kind, ToolArgumentErrorSchema)
	}
	if strings.Contains(err.Error(), "malformed JSON") {
		t.Errorf("valid JSON reported as malformed: %v", err)
	}
	if !strings.Contains(err.Error(), "top_k") || !strings.Contains(err.Error(), "integer") {
		t.Errorf("error does not name the field and expected type: %v", err)
	}
}

// A model that invents many stray fields must not push the one actionable
// diagnostic — the missing required field — out of the rendered message.
func TestArgsRequiredIssueSurvivesManyUnknownFields(t *testing.T) {
	contract := MustArgsContract("strict", String("zzz").Required().Desc("Required value."))
	payload := map[string]any{}
	for index := range 20 {
		payload[fmt.Sprintf("a%02d", index)] = index
	}
	raw, err := jsonv2.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	_, decodeErr := contract.Decode(string(raw))
	if decodeErr == nil {
		t.Fatal("missing required field must fail")
	}
	if !strings.Contains(decodeErr.Error(), "zzz") {
		t.Errorf("the actionable diagnostic was crowded out: %v", decodeErr)
	}
	if !strings.Contains(decodeErr.Error(), "not accepted") {
		t.Errorf("stray fields were not reported at all: %v", decodeErr)
	}
}

// Structured issues are bounded, not just the rendered string: a consumer
// walking Issues must not receive an unbounded blob of model-authored text.
func TestArgsIssuesAreBoundedIndependentlyOfError(t *testing.T) {
	contract := MustArgsContract("strict", String("a").Desc("Optional."))
	huge := strings.Repeat("x", 100_000)
	raw, err := jsonv2.Marshal(map[string]any{huge: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, decodeErr := contract.Decode(string(raw))
	if decodeErr == nil {
		t.Fatal("unknown field must fail")
	}
	var argumentError *ToolArgumentError
	if !errors.As(decodeErr, &argumentError) {
		t.Fatalf("error type = %T", decodeErr)
	}
	for _, issue := range argumentError.Issues {
		if len([]rune(issue.Message)) > maxToolArgumentMessageRunes {
			t.Errorf("issue message is %d runes, want bounded", len([]rune(issue.Message)))
		}
		if len([]rune(issue.Field)) > maxToolArgumentFieldRunes {
			t.Errorf("issue field is %d runes, want bounded", len([]rune(issue.Field)))
		}
	}
}
