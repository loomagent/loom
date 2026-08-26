package loom

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

type typedToolRequest struct {
	Query string `json:"query" jsonschema:"Search query." validate:"min=1,notblank" example:"loom agent runtime"`
	Limit int    `json:"limit,omitzero" validate:"omitempty,min=0"`
}

func TestNewToolBindsCompiledContract(t *testing.T) {
	contract, err := NewToolContract[typedToolRequest]("typed_search",
		WithArgumentDescription("limit", "Maximum results."),
		WithArgumentMaximum("limit", 5),
	)
	if err != nil {
		t.Fatal(err)
	}
	if contract.Name() != "typed_search" {
		t.Fatalf("name = %q", contract.Name())
	}

	tool := NewTool(contract, "Search.", func(_ context.Context, input typedToolRequest) (string, error) {
		return input.Query, nil
	}, WithRequiresNetwork())
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != contract.Name() || !info.RequiresNetwork {
		t.Fatalf("tool info = %+v", info)
	}
	if got := info.Parameters.Properties["limit"].Description; got != "Maximum results." {
		t.Fatalf("limit description = %q", got)
	}

	// The model-facing schema is a clone. Mutating it must not weaken the
	// validator compiled into the contract.
	maximum := float64(100)
	info.Name = "changed"
	info.Parameters.Properties["limit"].Maximum = &maximum
	freshInfo, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if freshInfo.Name != contract.Name() || freshInfo.Parameters.Properties["limit"].Maximum == nil || *freshInfo.Parameters.Properties["limit"].Maximum != 5 {
		t.Fatalf("tool info mutation leaked: %+v", freshInfo)
	}
	if _, err := tool.Invoke(context.Background(), `{"query":"loom","limit":6}`); err == nil || !strings.Contains(err.Error(), `"limit" must be at most 5`) {
		t.Fatalf("validation error = %v", err)
	}

	output, err := tool.Invoke(context.Background(), `{"query":"loom","limit":5}`)
	if err != nil || output != "loom" {
		t.Fatalf("output = %q, err = %v", output, err)
	}
}

func TestToolContractErrors(t *testing.T) {
	if _, err := NewToolContract[typedToolRequest](""); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("empty-name error = %v", err)
	}
	for _, name := range []string{" invalid", "invalid.name", "invalid-name", "InvalidName", "1st_tool", strings.Repeat("a", maxToolNameLength+1)} {
		if _, err := NewToolContract[typedToolRequest](name); err == nil || !strings.Contains(err.Error(), "invalid tool name") {
			t.Errorf("invalid name %q error = %v", name, err)
		}
	}
	if _, err := NewToolContract[typedToolRequest]("search", WithArgumentMaximum("missing", 1)); err == nil || !strings.Contains(err.Error(), `property "missing" does not exist`) {
		t.Fatalf("missing-property error = %v", err)
	}
}

func TestToolContractDecodeIncludesBoundName(t *testing.T) {
	contract := MustToolContract[typedToolRequest]("typed_search", WithArgumentMaximum("limit", 5))
	_, err := contract.Decode(`{"limit":1}`)
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatalf("error = %T %v", err, err)
	}
	if argumentError.Tool != contract.Name() {
		t.Fatalf("error tool = %q, want %q", argumentError.Tool, contract.Name())
	}
	if got, want := argumentError.ExampleArguments, `{"query":"loom agent runtime"}`; got != want {
		t.Fatalf("example arguments = %q, want %q", got, want)
	}
}

func TestToolContractOmitsIncompleteExample(t *testing.T) {
	type request struct {
		Query string `json:"query" example:"loom"`
		Mode  string `json:"mode"`
	}
	contract := MustToolContract[request]("incomplete")
	_, err := contract.Decode(`{}`)
	var argumentError *ToolArgumentError
	if !errors.As(err, &argumentError) {
		t.Fatal(err)
	}
	if argumentError.ExampleArguments != "" {
		t.Fatalf("incomplete example should be omitted: %q", argumentError.ExampleArguments)
	}
}

func TestToolContractRejectsInvalidExample(t *testing.T) {
	type schemaInvalid struct {
		Limit int `json:"limit" validate:"min=1,max=5" example:"9"`
	}
	if _, err := NewToolContract[schemaInvalid]("invalid_schema_example"); err == nil || !strings.Contains(err.Error(), "does not satisfy JSON Schema") {
		t.Fatalf("invalid schema example error = %v", err)
	}

	type validatorInvalid struct {
		Value   string `json:"value" validate:"eqfield=Confirm" example:"agent"`
		Confirm string `json:"confirm" example:"loom"`
	}
	if _, err := NewToolContract[validatorInvalid]("invalid_validator_example"); err == nil || !strings.Contains(err.Error(), "does not satisfy struct validation") {
		t.Fatalf("invalid validator example error = %v", err)
	}
}

func TestToolContractDecodeIsConcurrentSafe(t *testing.T) {
	contract := MustToolContract[typedToolRequest]("concurrent_search", WithArgumentMaximum("limit", 5))
	const workers = 32
	const iterations = 100
	errorsFound := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range iterations {
				got, err := contract.Decode(`{"query":"loom","limit":5}`)
				if err != nil {
					errorsFound <- err
					return
				}
				if got.Query != "loom" || got.Limit != 5 {
					errorsFound <- errors.New("decoded arguments changed during concurrent use")
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
}

func FuzzToolContractDecode(f *testing.F) {
	contract := MustToolContract[typedToolRequest]("fuzz_search", WithArgumentMaximum("limit", 5))
	for _, seed := range []string{
		`{"query":"loom","limit":5}`,
		`{"query":""}`,
		`{"limit":1}`,
		`{"query":"loom","limit":9007199254740993}`,
		`{"query":`,
		`null`,
		``,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_, err := contract.Decode(raw)
		if err == nil {
			return
		}
		var argumentError *ToolArgumentError
		if !errors.As(err, &argumentError) {
			t.Fatalf("Decode returned unnormalized error %T: %v", err, err)
		}
	})
}

func BenchmarkToolArgumentDecode(b *testing.B) {
	contract := MustToolContract[typedToolRequest]("benchmark_search", WithArgumentMaximum("limit", 5))
	const raw = `{"query":"loom","limit":5}`
	b.Run("compiled_contract", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := contract.Decode(raw); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("derive_per_call", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := DecodeToolArguments[typedToolRequest](raw); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("stdlib_v2", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var request typedToolRequest
			if err := jsonv2.Unmarshal([]byte(raw), &request); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// A schema the framework cannot exemplify on its own must still yield a usable
// contract: a required map has no valid empty instance, but the tool works.
func TestToolContractBuildsWhenExampleCannotBeAssembled(t *testing.T) {
	type requiredMap struct {
		Labels map[string]string `json:"labels" validate:"required"`
	}
	contract, err := NewToolContract[requiredMap]("required_map")
	if err != nil {
		t.Fatalf("NewToolContract: %v", err)
	}
	if _, err := contract.Decode(`{"labels":{"a":"b"}}`); err != nil {
		t.Fatalf("Decode(valid) = %v", err)
	}
	if _, err := contract.Decode(`{}`); err == nil {
		t.Fatal("Decode({}) must still reject a missing required field")
	}
}

// A validator bound with no JSON Schema equivalent must be left to the
// validator instead of failing contract construction.
func TestToolContractAcceptsNonNumericValidatorBound(t *testing.T) {
	type durationArgs struct {
		Timeout time.Duration `json:"timeout" validate:"min=1s,max=1h"`
	}
	if _, err := NewToolContract[durationArgs]("duration_tool"); err != nil {
		t.Fatalf("NewToolContract: %v", err)
	}
}

// An int64 bound float64 cannot hold exactly must not be projected as a
// rounded — and unsatisfiable — JSON Schema bound.
func TestToolContractSkipsInexactIntegerBound(t *testing.T) {
	type bigArgs struct {
		Big int64 `json:"big,omitempty" validate:"omitempty,min=9223372036854775806"`
	}
	contract, err := NewToolContract[bigArgs]("big_tool")
	if err != nil {
		t.Fatalf("NewToolContract: %v", err)
	}
	if _, err := contract.Decode(`{"big":9223372036854775806}`); err != nil {
		t.Fatalf("Decode(valid int64 bound) = %v", err)
	}
}

// Schema() must hand back a fully independent copy: mutating it — including
// plain slices and pointer bounds that CloneSchemas leaves shared — must not
// reach the schema the contract validates against.
func TestToolContractSchemaIsFullyIndependent(t *testing.T) {
	type simple struct {
		Query string `json:"query" validate:"required"`
	}
	contract := MustToolContract[simple]("mytool")

	first, second := contract.Schema(), contract.Schema()
	if len(first.Required) == 0 {
		t.Fatal("expected a required field to mutate")
	}
	if &first.Required[0] == &second.Required[0] {
		t.Fatal("two Schema() copies share the Required backing array")
	}

	first.Required[0] = "HACKED"
	if _, err := contract.Decode(`{"query":"x"}`); err != nil {
		t.Fatalf("mutating a returned schema changed contract validation: %v", err)
	}
}

// WithArgumentSchema discards everything configured before it, so ordering it
// after another schema option is a mistake worth catching rather than silently
// undoing that option.
func TestWithArgumentSchemaRejectsLateOrdering(t *testing.T) {
	type simple struct {
		Query string `json:"query" validate:"required"`
	}
	_, err := NewToolContract[simple]("late_schema",
		WithArgumentDescription("query", "described"),
		WithArgumentSchema(&jsonschema.Schema{Type: "object"}),
	)
	if err == nil || !strings.Contains(err.Error(), "must come before") {
		t.Fatalf("late WithArgumentSchema error = %v", err)
	}

	// Leading it is still fine.
	if _, err := NewToolContract[simple]("early_schema",
		WithArgumentSchema(&jsonschema.Schema{Type: "object"}),
	); err != nil {
		t.Fatalf("leading WithArgumentSchema = %v", err)
	}
}

// A len rule must not leave the two bounds sharing one int: adjusting the
// maximum later would drag the minimum with it.
func TestLenRuleBoundsAreNotAliased(t *testing.T) {
	type fixed struct {
		Code string `json:"code" validate:"len=4"`
	}
	schema, err := SchemaFor[fixed]()
	if err != nil {
		t.Fatal(err)
	}
	code := schema.Properties["code"]
	if code.MinLength == nil || code.MaxLength == nil {
		t.Fatalf("len did not set both bounds: %#v", code)
	}
	if code.MinLength == code.MaxLength {
		t.Fatal("minLength and maxLength share one pointer")
	}
	*code.MaxLength = 20
	if *code.MinLength != 4 {
		t.Fatalf("minLength moved to %d when maxLength was changed", *code.MinLength)
	}
}

// An or-rule with an unprojectable alternative must not emit an anyOf that
// matches everything while looking like a constraint.
func TestOrRuleWithUnprojectableAlternativeIsNotEmitted(t *testing.T) {
	type orArgs struct {
		P string `json:"p" validate:"required,url|startswith=/"`
	}
	schema, err := SchemaFor[orArgs]()
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range schema.Properties["p"].AllOf {
		if branch.AnyOf != nil {
			t.Fatalf("emitted a vacuous anyOf: %#v", branch.AnyOf)
		}
	}
	// The validator still enforces it.
	if _, err := DecodeToolArguments[orArgs](`{"p":"???"}`); err == nil {
		t.Error("validator must still reject a value matching neither alternative")
	}
	if _, err := DecodeToolArguments[orArgs](`{"p":"/ok"}`); err != nil {
		t.Errorf("a value matching an alternative must be accepted: %v", err)
	}
}
