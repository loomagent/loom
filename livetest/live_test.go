// Package livetest runs Loom against real provider endpoints. The suite is skipped unless
// LOOM_LIVE_ENV names a filled-in configuration file, so plain `go test ./...` stays hermetic
// and needs no credentials: only an operator who points the suite at a file reaches the
// network, and reaching it spends their money.
//
// The suite asserts what Loom promises a caller, not what a particular model happens to answer:
// a streamed turn still calls a tool whose arguments satisfy the tool's own contract, the
// assistant turn it produced is accepted back by the same endpoint, and a declared structured
// output survives validation. What varies by model, such as whether reasoning is streamed at
// all, is logged rather than asserted.
package livetest

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/loomagent/loom"
	"github.com/loomagent/loom/modelfactory"
)

const (
	// liveEnvOverride names the filled-in configuration file. It has no default: an absent or
	// forgotten variable must not be the difference between a hermetic run and a billed one.
	liveEnvOverride = "LOOM_LIVE_ENV"
	// liveCallBudget bounds every flow one model runs. A stalled endpoint fails the test
	// instead of holding it open.
	liveCallBudget = 4 * time.Minute
	// defaultEffort is the reasoning effort a block sends unless it overrides it.
	defaultEffort = "low"
	// jsonObjectPromptAttempts bounds the json_object phase: the documented retry gets one
	// chance to compensate for a model that did not follow the instruction first time.
	jsonObjectPromptAttempts = 2
	// livePrompt asks for the two things the flow needs in one turn: a tool call and an answer
	// worth reasoning about.
	livePrompt = "Echo the word 'chain', then say what 17*23 is."
)

// liveBlock is one provider block of live.env: the provider type, where to reach it, the
// credential to use, and the models to exercise through it.
type liveBlock struct {
	Provider modelfactory.Provider
	BaseURL  string
	APIKey   string
	// Effort is sent with every reasoning request this block makes. The empty value enables
	// reasoning without naming an effort, which is what a model that accepts no effort needs.
	Effort loom.ReasoningEffort
	// StructuredOutput is the capability this block declares for its structured call:
	// json_object by default, or json_schema for a model whose endpoint enforces a schema.
	// Declaring the stronger one is a claim the run checks: an endpoint that refuses
	// json_schema fails the model it was declared for.
	StructuredOutput loom.StructuredOutputMode
	Models           []string
}

// TestLiveProviders exercises every model configured by LOOM_LIVE_ENV.
//
// Each provider is a subtest and each of its models a subtest of that, so
// -run TestLiveProviders/openrouter selects one provider and
// -run 'TestLiveProviders/<provider>/<model>' one model.
func TestLiveProviders(t *testing.T) {
	path := os.Getenv(liveEnvOverride)
	if path == "" {
		t.Skipf("live provider tests are skipped: set %s to the path of a filled-in configuration file (see live.env.example)", liveEnvOverride)
	}
	blocks, err := loadLiveBlocks(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, block := range blocks {
		t.Run(block.Provider.String(), func(t *testing.T) {
			for _, model := range block.Models {
				t.Run(model, func(t *testing.T) {
					exerciseModel(t, block, model)
				})
			}
		})
	}
}

// exerciseModel runs one model through the flows Loom owns end to end: a streamed tool-calling
// turn, that turn carried back into a second request, and a structured-output call.
func exerciseModel(t *testing.T, block liveBlock, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveCallBudget)
	defer cancel()
	reasoning := loom.Reasoning{Mode: loom.ReasoningModeEnabled, Effort: block.Effort}

	// Capabilities stay undeclared so the request carries what this suite asks for rather than
	// what a declaration would gate it to.
	built, err := modelfactory.Build(modelfactory.Config{
		Provider: block.Provider,
		APIKey:   block.APIKey,
		BaseURL:  block.BaseURL,
		Model:    model,
	})
	if err != nil {
		t.Fatalf("build %s/%s: %v", block.Provider, model, err)
	}

	echo := loom.String("text").Required().Desc("The text to echo back.")
	contract := loom.MustArgsContract("echo", echo)
	registry := loom.NewToolRegistry(loom.NewArgsTool(contract, "Echo the given text.",
		func(_ context.Context, args loom.Args) (string, error) {
			return fmt.Sprintf(`{"echoed":%q}`, echo.Get(args)), nil
		}))
	tools, err := registry.InfoList(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	base := []loom.Message{{Role: loom.RoleUser, Content: livePrompt}}
	request := loom.ChatRequest{
		Messages:   base,
		Tools:      tools,
		ToolChoice: &loom.ToolChoice{Mode: loom.ToolChoiceAuto},
		Reasoning:  reasoning,
	}

	// A streamed turn puts the whole incremental path to work: frames in, a tool call out.
	var first *loom.ChatResponse
	if _, err := loom.Run(ctx, func(ctx context.Context, writer loom.TurnWriter, _ []loom.Turn, _ loom.UserMessage) error {
		var streamErr error
		first, streamErr = loom.StreamLLMToStep(ctx, writer, "live", built, request)
		return streamErr
	}, loom.RunOptions{ConversationID: "live"}); err != nil {
		t.Fatalf("stream a tool-calling turn: %v", err)
	}
	if first == nil || len(first.ToolCalls) == 0 {
		t.Fatalf("the streamed turn called no tool; finish reason %q, content %q",
			finishOf(first), contentOf(first))
	}
	call := first.ToolCalls[0]
	// Decoding the model's arguments against the tool's own contract is what Loom does before
	// it invokes the tool, and a live model is where a schema it cannot satisfy shows up.
	args, err := contract.Decode(call.Arguments)
	if err != nil {
		t.Fatalf("the model's tool call does not satisfy the tool's contract: %v", err)
	}
	if !echo.Present(args) {
		t.Errorf("the decoded call omits the required argument %q: %s", "text", call.Arguments)
	}
	t.Logf("streamed tool call: name=%s finish=%s arguments=%s reasoning=%dB details=%dB",
		call.Name, first.FinishReason, call.Arguments, len(first.ReasoningContent), len(first.ReasoningDetails))

	// Carrying the assistant turn back is what a tool loop does every iteration, and the
	// provider fields Loom preserves in it are exactly what the endpoint may require again.
	carried := loom.AppendAssistantTurn(base, first, []loom.ToolExecResult{
		{Call: call, Output: fmt.Sprintf(`{"echoed":%q}`, echo.Get(args))},
	})
	second, err := built.Chat(ctx, loom.ChatRequest{Messages: carried, Tools: tools, Reasoning: reasoning})
	if err != nil {
		t.Fatalf("the endpoint rejected the assistant turn carried back: %v", err)
	}
	if strings.TrimSpace(second.Content) == "" {
		t.Errorf("the carried-back turn produced no content; finish reason %q", second.FinishReason)
	}
	t.Logf("carried back: finish=%s content=%q", second.FinishReason, second.Content)

	exerciseStructuredOutput(t, ctx, block, model, reasoning)
}

// exerciseJSONObjectPrompt runs the composition the README recommends where no schema field
// exists: the contract's own instruction in a system message, the caller's task in a user
// message, and no format rules in the task.
//
// This is the only way a json_object request can work: the provider requires the prompt to contain
// the word "json" and refuses the request with a 400 rather than answering, and the suite used to
// pass only because Loom wrote that word into the prompt itself.
//
// It asserts the recipe, not one model's obedience. The first attempt satisfies the contract on
// a compliant model; when a model does not comply, the documented retry — the rejected output,
// the error, and the schema — gets one chance to, and the attempts that were needed are logged
// either way. Asserting "first attempt" would make this a coin flip on the model's mood, which
// is exactly why the caller owns the retry in the first place.
func exerciseStructuredOutput(t *testing.T, ctx context.Context, block liveBlock, model string, reasoning loom.Reasoning) {
	t.Helper()
	answer := loom.String("answer").Required().Desc("The numeric answer.").Example("17 × 23 = 391")
	certain := loom.Bool("certain").Required().Desc("Whether you are certain of the answer.").Example(true)
	contract := loom.MustArgsContract("verdict", answer, certain)
	schemaJSON, err := jsonv2.Marshal(contract.Schema())
	if err != nil {
		t.Fatalf("marshal contract schema: %v", err)
	}
	if example := contract.Example(); example == "" {
		t.Fatal("a contract whose arguments all declare examples must assemble one")
	}

	// The declared mode decides what goes out, and each mode asserts its own claim: json_object
	// carries no schema, so the prompt has to, while json_schema has to be accepted by the
	// endpoint because the declaration said it would be.
	built, err := modelfactory.Build(modelfactory.Config{
		Provider:     block.Provider,
		APIKey:       block.APIKey,
		BaseURL:      block.BaseURL,
		Model:        model,
		Capabilities: &loom.ModelCapabilities{StructuredOutput: block.StructuredOutput},
	})
	if err != nil {
		t.Fatalf("build %s/%s for json_object: %v", block.Provider, model, err)
	}

	messages := []loom.Message{{Role: loom.RoleUser, Content: "What is 17*23?"}}
	attempts := jsonObjectPromptAttempts
	if block.StructuredOutput == loom.StructuredOutputJSONObject {
		// The provider refuses a json_object request whose prompt never asks for JSON, so the
		// contract's own instruction leads.
		messages = append([]loom.Message{{Role: loom.RoleSystem, Content: contract.JSONObjectPrompt()}}, messages...)
	} else {
		// The endpoint enforces the schema, so the first attempt is the assertion.
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		args, response, callErr := loom.ChatStructuredArgs(ctx, "live.json_object", built,
			loom.ChatRequest{Messages: messages, Reasoning: reasoning}, contract)
		if callErr == nil {
			t.Logf("%s: attempt %d/%d satisfied the contract (model=%s)",
				block.StructuredOutput, attempt, attempts, response.Model)
			if !strings.Contains(answer.Get(args), "391") {
				t.Errorf("%s answered %q, want an answer containing %q", block.StructuredOutput, answer.Get(args), "391")
			}
			return
		}
		var invalid *loom.StructuredOutputError
		if !errors.As(callErr, &invalid) {
			t.Fatalf("%s: request-level failure on attempt %d: %v", block.StructuredOutput, attempt, callErr)
		}
		t.Logf("%s: attempt %d/%d rejected: %v", block.StructuredOutput, attempt, attempts, callErr)
		if attempt == attempts {
			break
		}
		messages = append(messages,
			loom.Message{Role: loom.RoleAssistant, Content: invalid.Content},
			loom.Message{Role: loom.RoleUser, Content: fmt.Sprintf(
				"Your previous output was rejected: %v\nReturn one JSON value matching:\n%s", callErr, schemaJSON)})
	}
	t.Errorf("%s: the contract was still unsatisfied after %d attempts", block.StructuredOutput, attempts)
}

// loadLiveBlocks reads path as KEY=VALUE lines and returns the provider blocks that
// LOOM_LIVE_PROVIDERS names, in that order.
//
// Configuration mistakes are reported rather than skipped: a block whose name is not a
// provider, or that lacks a key, an endpoint, or a model, would otherwise quietly reduce the
// run to fewer tests and look like a pass.
func loadLiveBlocks(path string) ([]liveBlock, error) {
	values, err := readEnvFile(path)
	if err != nil {
		return nil, err
	}
	names := splitList(values["LOOM_LIVE_PROVIDERS"])
	if len(names) == 0 {
		return nil, errors.New("LOOM_LIVE_PROVIDERS names no provider")
	}
	blocks := make([]liveBlock, 0, len(names))
	for _, name := range names {
		prefix := "LOOM_LIVE_" + strings.ToUpper(name)
		provider := modelfactory.Provider(name)
		if !provider.Valid() {
			return nil, fmt.Errorf("%s in LOOM_LIVE_PROVIDERS is not a provider; want one of %s",
				name, strings.Join(providerNames(), ", "))
		}
		block := liveBlock{
			Provider: provider,
			BaseURL:  strings.TrimSpace(values[prefix+"_URL"]),
			APIKey:   strings.TrimSpace(values[prefix+"_KEY"]),
			Effort:   defaultEffort,
			Models:   splitList(values[prefix+"_MODELS"]),
		}
		block.StructuredOutput = loom.StructuredOutputJSONObject
		if mode, declared := values[prefix+"_STRUCTURED_OUTPUT"]; declared {
			switch loom.StructuredOutputMode(strings.TrimSpace(mode)) {
			case loom.StructuredOutputJSONObject, loom.StructuredOutputJSONSchema:
				block.StructuredOutput = loom.StructuredOutputMode(strings.TrimSpace(mode))
			default:
				return nil, fmt.Errorf("%s_STRUCTURED_OUTPUT is %q, want %q or %q",
					prefix, mode, loom.StructuredOutputJSONObject, loom.StructuredOutputJSONSchema)
			}
		}
		if effort, declared := values[prefix+"_EFFORT"]; declared {
			block.Effort = loom.ReasoningEffort(strings.TrimSpace(effort))
			if block.Effort != loom.ReasoningEffortDefault && !loom.ValidReasoningEffort(block.Effort) {
				return nil, fmt.Errorf("%s_EFFORT is %q, want a token of letters, digits, '-' or '_'", prefix, block.Effort)
			}
		}
		switch {
		case block.APIKey == "":
			return nil, fmt.Errorf("%s_KEY is empty", prefix)
		case block.BaseURL == "":
			return nil, fmt.Errorf("%s_URL is empty", prefix)
		case !strings.HasPrefix(block.BaseURL, "https://") && !strings.HasPrefix(block.BaseURL, "http://"):
			return nil, fmt.Errorf("%s_URL is %q, want an http(s) endpoint", prefix, block.BaseURL)
		case len(block.Models) == 0:
			return nil, fmt.Errorf("%s_MODELS names no model", prefix)
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

// readEnvFile parses a KEY=VALUE file. Blank lines and lines whose first non-space character is
// '#' are ignored, values may be quoted, and a value may contain '='.
func readEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for index, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE, got %q", path, index+1, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, index+1)
		}
		values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values, nil
}

// splitList splits a comma-separated value, dropping empty elements so that a trailing comma is
// harmless.
func splitList(value string) []string {
	items := make([]string, 0, 4)
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// providerNames lists the providers a block may name, for error messages.
func providerNames() []string {
	values := modelfactory.ProviderValues()
	names := make([]string, 0, len(values))
	for _, provider := range values {
		names = append(names, provider.String())
	}
	return names
}

// finishOf and contentOf describe a missing response without dereferencing it.
func finishOf(response *loom.ChatResponse) loom.FinishReason {
	if response == nil {
		return "<nil>"
	}
	return response.FinishReason
}

func contentOf(response *loom.ChatResponse) string {
	if response == nil {
		return "<nil>"
	}
	return response.Content
}
