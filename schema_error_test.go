package loom

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	toolcontract "github.com/loomagent/loom/internal/toolcontract"
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

// A model only sees the tool description when it reasons about a call, so the
// summary has to describe every constraint the builder can declare.
func TestExpectedArgumentsSummarizeEveryConstraint(t *testing.T) {
	contract := MustArgsContract("t",
		Enum("mode", "fast", "slow").Required().Desc("Mode."),
		Float("ratio").Min(0).Max(1).Desc("Ratio."),
		Uint("count").Min(1).Desc("Count."),
		Uint("cap").Max(9).Desc("Cap."),
		Uint("after").ExclusiveMin(0).Desc("After."),
		Float("before").ExclusiveMax(1).Desc("Before."),
		String("name").MinLen(1).MaxLen(5).Desc("Name."),
		String("note").NotBlank().Desc("Note."),
		String("tag").Pattern("^ab").Desc("Tag."),
		String("id").Pattern("^a\\.b").Desc("Id."),
		String("code").Pattern("^a.c$").Desc("Code."),
		String("day").Format("date").Desc("Day."),
		Strings("tags").MinItems(1).MaxItems(3).Unique().Desc("Tags."),
	)
	_, err := contract.Decode(`{"mode":7}`)
	if err == nil {
		t.Fatal("a declared enum must reject a number")
	}
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatalf("error type = %T", err)
	}

	// NotBlank is a schema constraint now, so the model sees it before it calls. It
	// lives in an allOf branch beside anything else the argument declares.
	for _, want := range []string{
		`mode=<string, required, one of ["fast","slow"]>`,
		`ratio=<number, optional, 0..1>`,
		`count=<integer, optional, >=1>`,
		`cap=<integer, optional, 0..9>`,
		`after=<integer, optional, >=0, >0>`,
		`before=<number, optional, <1>`,
		`name=<string, optional, min length 1, max length 5>`,
		`note=<string, optional, non-blank>`,
		`tag=<string, optional, starts with "ab">`,
		`id=<string, optional, starts with "a.b">`,
		`code=<string, optional, pattern "^a.c$">`,
		`day=<string, optional, format date>`,
		`tags=<array, optional, min items 1, max items 3, unique items>`,
	} {
		if !strings.Contains(argumentError.ExpectedArguments, want) {
			t.Errorf("expected arguments are missing %s:\n%s", want, argumentError.ExpectedArguments)
		}
	}
	if argumentError.Unwrap() == nil {
		t.Fatal("the underlying failure is not reachable")
	}
}

// A pattern becomes prose when it can: a model fixes "must start with" far more
// reliably than it fixes a regular expression. NotBlank is a rule rather than a
// pattern, so it renders through the custom-validation path.
func TestStringConstraintIssuesDescribeTheShape(t *testing.T) {
	cases := []struct {
		name     string
		argument Declaration
		value    string
		want     string
	}{
		{"not blank", String("q").NotBlank().Desc("Query."), "   ", "must not be blank"},
		{"starts with", String("q").Pattern("^ab").Desc("Query."), "zz", `must start with "ab"`},
		{"ends with", String("q").Pattern("ab$").Desc("Query."), "zz", `must end with "ab"`},
		{"contains", String("q").Pattern("ab").Desc("Query."), "zz", `must contain "ab"`},
		{"regexp", String("q").Pattern("^a.c$").Desc("Query."), "zz", `must match pattern "^a.c$"`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			contract := MustArgsContract("t", testCase.argument)
			raw, err := jsonv2.Marshal(map[string]any{"q": testCase.value})
			if err != nil {
				t.Fatal(err)
			}
			_, decodeErr := contract.Decode(string(raw))
			if decodeErr == nil {
				t.Fatal("the value violates the declared pattern")
			}
			if !strings.Contains(decodeErr.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to contain %q", decodeErr, testCase.want)
			}
		})
	}
}

// Every code the validator can emit must become a readable, field-named issue,
// and a code it does not emit yet must still render: a model that receives a
// blank line cannot fix its call.
func TestValidationIssueRendersEveryValidatorCode(t *testing.T) {
	cases := []struct {
		code   string
		field  string
		params map[string]any
	}{
		{"missing_required_property", "n.child", map[string]any{"property": "child"}},
		{"additional_property_mismatch", "n.child", map[string]any{"property": "child"}},
		{"value_above_maximum", "n", map[string]any{"maximum": 9}},
		{"value_below_minimum", "n", map[string]any{"minimum": 1}},
		{"exclusive_maximum_mismatch", "n", map[string]any{"exclusive_maximum": 10}},
		{"exclusive_minimum_mismatch", "n", map[string]any{"exclusive_minimum": 0}},
		{"value_not_in_enum", "n", map[string]any{"allowed": []any{"a", "b"}}},
		{"const_mismatch", "n", map[string]any{"expected": "a"}},
		{"unique_items_mismatch", "n", nil},
		{"pattern_mismatch", "n", map[string]any{"pattern": "^a"}},
		{"string_too_short", "n", map[string]any{"min_length": 1}},
		{"string_too_long", "n", map[string]any{"max_length": 5}},
		{"items_too_short", "n", map[string]any{"min_items": 1}},
		{"items_too_long", "n", map[string]any{"max_items": 5}},
		{"type_mismatch", "n", map[string]any{"expected": "integer", "received": "string"}},
	}
	for _, testCase := range cases {
		issue := validationIssue("n", toolcontract.Violation{
			Keyword: "keyword",
			Code:    testCase.code,
			Params:  testCase.params,
		})
		if issue.Message == "" {
			t.Errorf("%s rendered no message", testCase.code)
		}
		if issue.Code != testCase.code {
			t.Errorf("%s became code %q", testCase.code, issue.Code)
		}
		if issue.Field != testCase.field {
			t.Errorf("%s became field %q, want %q", testCase.code, issue.Field, testCase.field)
		}
	}

	future := validationIssue("n", toolcontract.Violation{Keyword: "futureKeyword", Code: "future_code"})
	if !strings.Contains(future.Message, "futureKeyword") {
		t.Fatalf("an unknown code rendered %q", future.Message)
	}
}

// A declared example that does not satisfy its own argument is an authoring
// mistake: it must fail when the contract is built, not when a model copies it.
func TestDeclaredExampleMustSatisfyItsOwnArgument(t *testing.T) {
	_, err := NewArgsContract("t", String("q").Pattern("^a").Example("zz").Desc("Query."))
	if err == nil {
		t.Fatal("an example that violates its own pattern must fail the build")
	}
	if !strings.Contains(err.Error(), "q.examples[0]") {
		t.Fatalf("the error does not point at the example: %v", err)
	}
}

// A format the contract projects a pattern for is described in prose. Handing the model
// the regular expression the builder derived tells it less, and in a form it has to
// decode first.
func TestFormatViolationNamesTheFormatNotThePattern(t *testing.T) {
	contract := MustArgsContract("t", Date("day").Desc("Day."))
	_, err := contract.Decode(`{"day":"17/08/2026"}`)
	if err == nil {
		t.Fatal("a value that is not a date must fail")
	}
	if !strings.Contains(err.Error(), "must be a date (YYYY-MM-DD)") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), `\d`) {
		t.Fatalf("the model was handed the regular expression: %v", err)
	}
}

// An explicit pattern is a constraint of its own, so it is still reported as one even
// when the argument also carries a format.
func TestExplicitPatternIsStillReported(t *testing.T) {
	contract := MustArgsContract("t", Date("day").Pattern(`^\d{4}-01-01$`).Desc("New year's day."))
	_, err := contract.Decode(`{"day":"2026-08-17"}`)
	if err == nil {
		t.Fatal("a value outside the explicit pattern must fail")
	}
	if !strings.Contains(err.Error(), "must match pattern") {
		t.Fatalf("error = %v", err)
	}
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatalf("error type = %T", err)
	}
	if !strings.Contains(argumentError.ExpectedArguments, `pattern "^\\d{4}-01-01$"`) {
		t.Fatalf("an explicit pattern must still appear in the summary: %s", argumentError.ExpectedArguments)
	}
}
