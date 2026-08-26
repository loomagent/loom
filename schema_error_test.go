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

	// items is "omitempty,min=2": an empty array satisfies the validator, so
	// minItems is deliberately not projected and the bound is enforced by the
	// validator instead. Bounds an empty value does satisfy — limit's max, and
	// meta's — stay in the schema, as asserted above.
	if message, exists := got["items:min"]; exists {
		t.Errorf("items:min = %q, want no schema-level bound for an omitempty field", message)
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

// A well-formed JSON document whose value does not fit the target field must be
// reported as a type problem, not as malformed JSON, and must not leak the Go
// struct name into a model-facing message.
func TestDecodeReportsTypeMismatchRatherThanMalformedJSON(t *testing.T) {
	type webSearchArgs struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k,omitempty"`
	}
	loose := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"query": {Type: "string"},
			"top_k": {},
		},
	}
	_, err := DecodeToolArgumentsWithSchemaFor[webSearchArgs]("web_search", `{"query":"go","top_k":"5"}`, loose)
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
	if strings.Contains(err.Error(), "webSearchArgs") {
		t.Errorf("Go struct name leaked to the model: %v", err)
	}
	if !strings.Contains(err.Error(), "top_k") || !strings.Contains(err.Error(), "integer") {
		t.Errorf("error does not name the field and expected type: %v", err)
	}
}

// datetime=<layout> must project the format its layout actually describes, and
// the diagnostic must name the layout so the model can correct itself.
func TestDatetimeLayoutProjectsFormatAndNamesLayout(t *testing.T) {
	type dateArgs struct {
		When string `json:"when" validate:"required,datetime=2006-01-02"`
	}
	schema, err := SchemaFor[dateArgs]()
	if err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties["when"].Format; got != "date" {
		t.Errorf("when format = %q, want date for a date-only layout", got)
	}
	_, err = DecodeToolArgumentsFor[dateArgs]("t", `{"when":"2026-08-25T10:00:00Z"}`)
	if err == nil {
		t.Fatal("a timestamp must not satisfy a date-only layout")
	}
	if !strings.Contains(err.Error(), "2006-01-02") {
		t.Errorf("diagnostic does not name the layout: %v", err)
	}
	if _, err := DecodeToolArgumentsFor[dateArgs]("t", `{"when":"2026-08-25"}`); err != nil {
		t.Errorf("a value matching the layout must be accepted: %v", err)
	}
}

// An or-rule's tag already carries its argument; appending Param again produced
// constraints like "url|startswith=/=/".
func TestOrRuleDiagnosticIsNotMangled(t *testing.T) {
	type orArgs struct {
		P string `json:"p" validate:"required,url|startswith=/"`
	}
	_, err := DecodeToolArgumentsFor[orArgs]("t", `{"p":"???"}`)
	if err == nil {
		t.Fatal("value satisfying neither alternative must fail")
	}
	if strings.Contains(err.Error(), "=/=/") {
		t.Errorf("or-rule rendered with a duplicated argument: %v", err)
	}
}

// Arguments that are not a struct have no struct rules to run; they must not be
// reported as a misconfigured tool.
func TestNonStructArgumentsSkipStructValidation(t *testing.T) {
	if _, err := DecodeToolArgumentsFor[[]string]("t", `["a"]`); err != nil {
		t.Errorf("slice arguments = %v, want accepted", err)
	}
	type ptrArgs struct {
		A string `json:"a,omitempty"`
	}
	if _, err := DecodeToolArgumentsFor[*ptrArgs]("t", `null`); err != nil {
		t.Errorf("null into a pointer argument = %v, want accepted", err)
	}
}

// A key that violates propertyNames must not read like a value violation.
func TestMapKeyViolationIsLabelledAsKey(t *testing.T) {
	type mapArgs struct {
		A string            `json:"a"`
		M map[string]string `json:"m,omitempty" validate:"omitempty,dive,keys,min=3,endkeys,min=10"`
	}
	_, err := DecodeToolArgumentsFor[mapArgs]("t", `{"a":"x","m":{"ab":"0123456789"}}`)
	if err == nil {
		t.Fatal("a too-short key must fail")
	}
	if !strings.Contains(err.Error(), "field name") {
		t.Errorf("key violation not labelled as a key: %v", err)
	}
}

// A model that invents many stray fields must not push the one actionable
// diagnostic — the missing required field — out of the rendered message.
func TestRequiredIssueSurvivesManyUnknownFields(t *testing.T) {
	type strictArgs struct {
		Zzz string `json:"zzz" validate:"required"`
	}
	payload := map[string]any{}
	for index := range 20 {
		payload[fmt.Sprintf("a%02d", index)] = index
	}
	raw, err := jsonv2.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	_, decodeErr := DecodeToolArgumentsFor[strictArgs]("t", string(raw))
	if decodeErr == nil {
		t.Fatal("missing required field must fail")
	}
	if !strings.Contains(decodeErr.Error(), `"zzz" is required`) {
		t.Errorf("the actionable diagnostic was crowded out: %v", decodeErr)
	}
	if !strings.Contains(decodeErr.Error(), "not accepted") {
		t.Errorf("stray fields were not reported at all: %v", decodeErr)
	}
}

// Structured issues are bounded too, not just the rendered string: a consumer
// walking Issues must not receive an unbounded blob of model-authored text.
func TestIssuesAreBoundedIndependentlyOfError(t *testing.T) {
	type strictArgs struct {
		A string `json:"a,omitempty"`
	}
	huge := strings.Repeat("x", 100_000)
	raw, err := jsonv2.Marshal(map[string]any{huge: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, decodeErr := DecodeToolArgumentsFor[strictArgs]("t", string(raw))
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
