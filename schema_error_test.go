package loom

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
)

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
		Uint("top_k").Desc("Result count."),
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
