// Structured model calls require complete JSON responses that satisfy the same
// declared contract used for tool arguments, with the model output in place of
// the model input.
package loom

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

const defaultStructuredOutputAttempts uint64 = 2

// maxStructuredOutputNameLen is deliberately an untyped constant, so it can be
// compared with int values. Do not give it an explicit uint64 type, which will not
// compile, and do not fold it into the typed constant group above, which trips
// SA9004.
const maxStructuredOutputNameLen = 64

// StructuredOption configures the structured output and output retry of
// ChatStructuredArgs.
type StructuredOption func(*structuredConfig)

type structuredConfig struct {
	name        string
	description string
	maxAttempts uint64
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

// WithStructuredMaxAttempts sets how many attempts are made when the output does
// not satisfy the contract.
func WithStructuredMaxAttempts(maxAttempts uint64) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.maxAttempts = maxAttempts
	}
}

// WithStructuredValidator adds a business check after contract validation; a
// failure there also triggers an output retry.
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
// JSON, or failed the local contract or a business check.
type StructuredOutputError struct {
	Attempt uint64
	Content string
	Err     error
}

func (e *StructuredOutputError) Error() string {
	return fmt.Sprintf("structured output attempt %d invalid: %v", e.Attempt, e.Err)
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
// only supports json_object falls back to a JSON object plus a prompt constraint.
// The output is always validated locally against the contract, and the whole
// response must be a single valid JSON value. Business constraints a schema cannot
// express go through WithStructuredValidator.
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

	cfg := structuredConfig{maxAttempts: defaultStructuredOutputAttempts}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.maxAttempts == 0 {
		cfg.maxAttempts = 1
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

	var lastResp *ChatResponse
	var lastErr error
	for attempt := uint64(1); attempt <= cfg.maxAttempts; attempt++ {
		callOptions := append([]CallModelOption{}, cfg.callOptions...)
		callOptions = append(callOptions, withCallModelRequestForModel(func(current ChatModel) ChatRequest {
			callReq := withStructuredOutputRequest(req, current.Capabilities(), cfg.name, cfg.description, schema)
			if attempt > 1 {
				callReq.Messages = withStructuredRetryMessages(callReq.Messages, lastResp, lastErr)
			}
			return callReq
		}))
		resp, err := CallModel(ctx, purpose, model, req, callOptions...)
		if err != nil {
			return Args{}, resp, err
		}
		lastResp = resp
		if resp.FinishReason == FinishReasonLength {
			lastErr = &StructuredOutputError{
				Attempt: attempt,
				Content: resp.Content,
				Err:     fmt.Errorf("finish_reason=%s", resp.FinishReason),
			}
			continue
		}
		args, err := contract.DecodeContext(ctx, resp.Content)
		if err == nil && cfg.validate != nil {
			err = cfg.validate(args)
		}
		if err == nil {
			return args, resp, nil
		}
		lastErr = &StructuredOutputError{Attempt: attempt, Content: resp.Content, Err: err}
	}
	return Args{}, lastResp, lastErr
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

func withStructuredOutputRequest(req ChatRequest, caps ModelCapabilities, name, description string, schema *Schema) ChatRequest {
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
		req.Messages = appendStructuredPrompt(req.Messages, schema, description)
	case StructuredOutputNone, StructuredOutputUnsupported:
		// Explicitly unsupported, or undeclared: both fall back to writing the schema
		// into the prompt as plain text
		req.ResponseFormat = ResponseFormatDefault
		req.StructuredOutput = nil
		req.Messages = appendStructuredPrompt(req.Messages, schema, description)
	default:
		req.ResponseFormat = ResponseFormatDefault
		req.StructuredOutput = nil
		req.Messages = appendStructuredPrompt(req.Messages, schema, description)
	}
	return req
}

func appendStructuredPrompt(messages []Message, schema *Schema, description string) []Message {
	schemaJSON, err := jsonv2.Marshal(schema, jsontext.WithIndent("  "))
	if err != nil {
		schemaJSON = []byte("{}")
	}
	content := fmt.Sprintf(`Output only one JSON value that satisfies the JSON Schema below. Do not output Markdown or an explanation.

Description:
%s

JSON Schema:
%s`, description, string(schemaJSON))
	out := append([]Message{}, messages...)
	out = append(out, Message{Role: RoleSystem, Content: content})
	return out
}

func withStructuredRetryMessages(messages []Message, resp *ChatResponse, err error) []Message {
	out := append([]Message{}, messages...)
	if resp != nil && strings.TrimSpace(resp.Content) != "" {
		out = append(out, Message{
			Role:    RoleAssistant,
			Content: trimForRetry(resp.Content, 4000),
		})
	}
	out = append(out, Message{
		Role: RoleUser,
		Content: fmt.Sprintf(`The previous output did not satisfy the structured-output requirements: %v

Output the complete JSON again. Output JSON only, with no Markdown and no explanation.`, err),
	})
	return out
}

func trimForRetry(s string, limit int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "\n...(truncated)"
}
