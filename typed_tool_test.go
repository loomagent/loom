package loom

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type typedToolRequest struct {
	Query string `json:"query" jsonschema:"Search query." validate:"min=1,notblank" example:"loom agent runtime"`
	Limit int    `json:"limit,omitempty" validate:"omitempty,min=0"`
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
	info.Parameters.Properties["limit"].Maximum = &maximum
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
}
