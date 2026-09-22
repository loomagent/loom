// Structured model calls require complete JSON responses that satisfy the same
// declared contract used for tool arguments, with the model output in place of
// the model input.
package loom

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// maxStructuredOutputNameLen is deliberately an untyped constant, so it can be
// compared with int values.
const maxStructuredOutputNameLen = 64

// StructuredOption configures the structured output call of ChatStructuredArgs.
type StructuredOption func(*structuredConfig)

type structuredConfig struct {
	name        string
	description string
	validate    func(Args) error
	callOptions []CallModelOption
}

// WithStructuredName sets the response_format name sent to the provider. The
// contract's name is used by default.
func WithStructuredName(name string) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.name = name
	}
}

// WithStructuredDescription sets the description sent to the provider and included
// in the prompt.
func WithStructuredDescription(description string) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.description = description
	}
}

// WithStructuredValidator adds a business check after contract validation. A
// failure there is reported like any other invalid output.
func WithStructuredValidator(validate func(Args) error) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.validate = validate
	}
}

// WithStructuredCallOptions passes CallModel options through, such as per-call
// failover.
func WithStructuredCallOptions(opts ...CallModelOption) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.callOptions = append(cfg.callOptions, opts...)
	}
}

// WithStructuredFailover enables model failover for this structured call.
func WithStructuredFailover(cfg FailoverConfig) StructuredOption {
	return WithStructuredCallOptions(WithModelFailover(cfg))
}

// StructuredOutputError means the model's output was incomplete, was not valid
// JSON, or failed the local contract or a business check. It carries the response
// so the caller decides what happens next: how many times to ask again, and what
// context and feedback to send with it, is the caller's policy. A react turn
// answers that by putting the error into the conversation beside everything else
// that is known.
type StructuredOutputError struct {
	Content string
	Err     error
}

func (e *StructuredOutputError) Error() string {
	return fmt.Sprintf("structured output invalid: %v", e.Err)
}

func (e *StructuredOutputError) Unwrap() error {
	return e.Err
}

// ChatStructuredArgs calls the model and returns Args once the output has been
// validated against the contract.
//
// It mirrors the tool-argument contract: the same ArgsContract declares the fields,
// the constraints, and the descriptions, except that here it constrains what the
// model returns rather than what it sends. Reading works the same way, through the
// typed handles fixed at declaration:
//
//	summary := loom.String("summary").MinLen(1).MaxLen(200).Desc("review summary")
//
// A provider with native json_schema support receives this same schema; one that
// only supports json_object receives a JSON object request. Loom never writes the
// schema, or anything else the caller did not write, into the prompt: a request that
// constrains the model by rewriting the conversation is a hidden one. So on a model
// that declares no structured-output support this fails, and on a model whose
// capability is undeclared the request goes out as the caller wrote it — the output
// is still validated locally against the contract, and an invalid response comes
// back with the response attached so the caller can decide what to send next.
func ChatStructuredArgs(
	ctx context.Context,
	purpose string,
	model ChatModel,
	req ChatRequest,
	contract *ArgsContract,
	opts ...StructuredOption,
) (Args, *ChatResponse, error) {
	if model == nil {
		return Args{}, nil, errors.New("loom.ChatStructuredArgs: model must not be nil")
	}
	if contract == nil {
		return Args{}, nil, errors.New("loom.ChatStructuredArgs: contract must not be nil")
	}

	cfg := structuredConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.name == "" {
		cfg.name = contract.Name()
	}
	cfg.name = NormalizeStructuredOutputName(cfg.name)
	if cfg.description == "" {
		cfg.description = "structured response"
	}

	// The provider sees a private clone; the contract keeps validating against
	// its own copy, so a provider SDK cannot mutate what DecodeContext enforces.
	schema := contract.Schema()

	// One call. A failure is reported rather than retried, because how many times to ask
	// again, and what to send the second time, is the caller's policy: a react turn answers
	// that by putting the error into the conversation beside everything else it knows.
	callOptions := append([]CallModelOption{}, cfg.callOptions...)
	callOptions = append(callOptions, withCallModelRequestForModel(func(current ChatModel) (ChatRequest, error) {
		return withStructuredOutputRequest(req, current.Capabilities(), cfg.name, cfg.description, schema)
	}))
	resp, err := CallModel(ctx, purpose, model, req, callOptions...)
	if err != nil {
		return Args{}, resp, err
	}
	if resp.FinishReason == FinishReasonLength {
		return Args{}, resp, &StructuredOutputError{
			Content: resp.Content,
			Err:     fmt.Errorf("finish_reason=%s", resp.FinishReason),
		}
	}
	args, err := contract.DecodeContext(ctx, resp.Content)
	if err == nil && cfg.validate != nil {
		err = cfg.validate(args)
	}
	if err != nil {
		return Args{}, resp, &StructuredOutputError{Content: resp.Content, Err: err}
	}
	return args, resp, nil
}

// NormalizeStructuredOutputName builds a response_format name a provider accepts.
func NormalizeStructuredOutputName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "structured_output"
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range name {
		var write rune
		switch {
		case r == '-' || r == '_':
			write = r
		case r >= 'a' && r <= 'z':
			write = r
		case r >= 'A' && r <= 'Z':
			write = r
		case r >= '0' && r <= '9':
			write = r
		default:
			write = '_'
		}
		if write == '_' {
			if lastUnderscore {
				continue
			}
			lastUnderscore = true
		} else {
			lastUnderscore = false
		}
		b.WriteRune(write)
		if b.Len() >= maxStructuredOutputNameLen {
			break
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		return "structured_output"
	}
	if len(out) > maxStructuredOutputNameLen {
		out = out[:maxStructuredOutputNameLen]
		out = strings.Trim(out, "_-")
	}
	if out == "" {
		return "structured_output"
	}
	return out
}

// StructuredSchemaObject turns a schema into a plain JSON object, which a provider
// SDK can place in an interface{} field.
func StructuredSchemaObject(schema *Schema) (map[string]any, error) {
	if schema == nil {
		return nil, errors.New("schema must not be nil")
	}
	data, err := jsonv2.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := jsonv2.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func withStructuredOutputRequest(req ChatRequest, caps ModelCapabilities, name, description string, schema *Schema) (ChatRequest, error) {
	switch caps.StructuredOutput {
	case StructuredOutputJSONSchema:
		req.ResponseFormat = ResponseFormatDefault
		req.StructuredOutput = &StructuredOutput{
			Mode:        StructuredOutputJSONSchema,
			Name:        name,
			Description: description,
			Schema:      schema,
		}
	case StructuredOutputJSONObject:
		req.ResponseFormat = ResponseFormatJSONObject
		req.StructuredOutput = &StructuredOutput{Mode: StructuredOutputJSONObject}
	case StructuredOutputNone:
		// A model that declares no structured-output support cannot be constrained by this
		// call, and rewriting the prompt to compensate would hide the request from whoever
		// wrote the conversation. Fail instead of calling more quietly.
		return ChatRequest{}, fmt.Errorf("loom: model declares structured_output=%q, so it cannot be asked for structured output", caps.StructuredOutput)
	default:
		// Undeclared capability: the request goes out exactly as the caller wrote it, the
		// same way an undeclared capability passes through everywhere else. The contract
		// still validates the response locally, and an invalid response is returned with the
		// response attached, for the caller to decide what to send next.
		req.ResponseFormat = ResponseFormatDefault
		req.StructuredOutput = nil
	}
	return req, nil
}
