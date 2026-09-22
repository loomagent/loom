// Command structured runs the structured-output path against a scripted model: the first answer
// is refused, the caller adds the error and the schema, and the second answer satisfies the
// contract.
//
// It needs no API key, and it prints what the model was asked, because the two things it exists
// to show are what Loom sent and what Loom left alone.
//
//	go run ./examples/structured
package main

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/loomagent/loom"
)

func main() {
	ctx := context.Background()

	// One declaration per field, read back through the handle: it is the schema the model is
	// shown, the validator its answer is checked against, and the typed accessor the caller
	// reads. Example values are not decoration: they are validated against the same schema when
	// the contract is built, and they are what the prompt below shows.
	verdict := loom.Enum("verdict", "positive", "negative", "mixed").
		Required().Example("mixed").Desc("Overall sentiment of the review.")
	confidence := loom.Float("confidence").
		Required().Min(0).Max(1).Example(0.6).Desc("How sure the judgement is, from 0 to 1.")
	notes := loom.String("notes").
		Required().MinLen(1).MaxLen(80).Example("fast but damaged").Desc("One line of supporting detail.")
	contract := loom.MustArgsContract("sentiment", verdict, confidence, notes)

	// A json_object endpoint carries no schema, so the prompt has to carry the shape — and the
	// provider may refuse the request outright without the word "json" in it. The contract builds
	// that instruction from the declarations it already validated.
	fmt.Println("the instruction a json_object prompt needs, written by the contract:")
	fmt.Println(indent(contract.JSONObjectPrompt()))

	// The scripted model stands in for a provider: it declares json_object, so no schema travels,
	// and its first answer invents a shape of its own.
	model := &scriptedModel{
		capabilities: loom.ModelCapabilities{StructuredOutput: loom.StructuredOutputJSONObject},
		responses: []string{
			`{"sentiment":"mixed","reason":"fast but damaged"}`,
			`{"verdict":"mixed","confidence":0.9,"notes":"fast but damaged"}`,
		},
	}

	// The caller owns the prompt, so the caller writes the task; the contract's instruction is the
	// part that has to agree with what will be enforced.
	messages := []loom.Message{
		{Role: loom.RoleSystem, Content: contract.JSONObjectPrompt()},
		{Role: loom.RoleUser, Content: "Judge the sentiment of: 'The delivery was fast but the box was damaged.'"},
	}
	schemaJSON, err := jsonv2.Marshal(contract.Schema())
	if err != nil {
		fail(err)
	}

	// Loom asks once and reports what happened: how many times to ask again, and what to send the
	// second time, is the caller's decision, because only the caller knows what context is worth
	// paying for.
	const attempts = 2
	for attempt := 1; attempt <= attempts; attempt++ {
		args, _, err := loom.ChatStructuredArgs(ctx, "example.structured", model, loom.ChatRequest{
			Messages:  messages,
			Reasoning: loom.Reasoning{Mode: loom.ReasoningModeDisabled},
		}, contract)
		if err == nil {
			fmt.Printf("attempt %d satisfied the contract: verdict=%q confidence=%v notes=%q\n",
				attempt, verdict.Get(args), confidence.Get(args), notes.Get(args))
			fmt.Println()
			break
		}
		var invalid *loom.StructuredOutputError
		if !errors.As(err, &invalid) {
			// Anything else came from the request itself, where the provider's retry schedule
			// already did its work. Conflating the two is how a caller retries the wrong thing.
			fail(fmt.Errorf("attempt %d failed before the model answered: %w", attempt, err))
		}
		fmt.Printf("attempt %d was refused, and the answer came back with the error:\n  %v\n  the model sent: %s\n",
			attempt, err, invalid.Content)
		if attempt == attempts {
			fmt.Println("stopping at the caller's limit: another attempt would cost more than it is worth here")
			fmt.Println()
			break
		}
		// What to send next is the caller's choice — the rejected answer, why it was rejected, the
		// schema that will be enforced. Loom adds none of it, which is why the second request below
		// is visibly the caller's work.
		messages = append(messages,
			loom.Message{Role: loom.RoleAssistant, Content: invalid.Content},
			loom.Message{Role: loom.RoleUser, Content: fmt.Sprintf(
				"Your previous output was rejected: %v\nReturn one JSON value matching:\n%s", err, schemaJSON)})
	}

	// What the model was actually asked.
	fmt.Println("what the model received:")
	for index, request := range model.requests {
		fmt.Printf("  call %d: messages=%d (%s)  response_format=%s  structured_output=%s\n",
			index+1, len(request.Messages), roles(request.Messages), request.ResponseFormat, mode(request.StructuredOutput))
	}
	if len(model.requests) == 0 {
		fail(errors.New("the scripted model was never called"))
	}
	if model.requests[0].Messages[0].Content != contract.JSONObjectPrompt() {
		fail(errors.New("Loom rewrote the prompt; it must not"))
	}
	fmt.Println("  the first request carried the caller's own messages, unchanged.")
}

// scriptedModel replays the answers a provider would return and records every request, so the
// example can show both sides.
type scriptedModel struct {
	capabilities loom.ModelCapabilities
	responses    []string
	requests     []loom.ChatRequest
}

func (m *scriptedModel) Name() string { return "example/scripted" }

func (m *scriptedModel) Capabilities() loom.ModelCapabilities { return m.capabilities }

func (m *scriptedModel) Chat(_ context.Context, request loom.ChatRequest) (*loom.ChatResponse, error) {
	m.requests = append(m.requests, request)
	if len(m.responses) == 0 {
		return nil, errors.New("the script has run out of answers")
	}
	content := m.responses[0]
	m.responses = m.responses[1:]
	return &loom.ChatResponse{Content: content, FinishReason: loom.FinishReasonStop}, nil
}

func (m *scriptedModel) Stream(context.Context, loom.ChatRequest) (loom.Stream, error) {
	return nil, errors.New("this example calls synchronously; examples/react shows the streaming loop")
}

func roles(messages []loom.Message) string {
	names := make([]string, 0, len(messages))
	for _, message := range messages {
		names = append(names, string(message.Role))
	}
	return strings.Join(names, ",")
}

func mode(output *loom.StructuredOutput) string {
	if output == nil {
		return "none"
	}
	return string(output.Mode)
}

func indent(text string) string {
	return "  " + strings.ReplaceAll(text, "\n", "\n  ") + "\n"
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "example/structured:", err)
	os.Exit(1)
}
