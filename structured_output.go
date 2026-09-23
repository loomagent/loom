// Structured model calls require complete JSON responses that satisfy the same
// declared contract used for tool arguments, with the model output in place of
// the model input.
package loom

import (
	"context"
	stdjson "encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

// maxStructuredOutputNameLen is deliberately an untyped constant, so it can be
// compared with int values.
const maxStructuredOutputNameLen = 64

// StructuredOption configures the structured output call of ChatStructuredArgs.
type StructuredOption func(*structuredConfig)

type structuredConfig struct {
	name           string
	description    string
	validate       func(Args) error
	callOptions    []CallModelOption
	attempts       uint64
	attemptTimeout time.Duration
	nextRequest    func(context.Context, StructuredAttempt) (*ChatRequest, error)
}

// StructuredAttempt describes one finished attempt, for the caller that decides what the next
// request says.
type StructuredAttempt struct {
	// Number is the attempt that just finished, counting from one.
	Number uint64
	// Request is the logical request that attempt sent, so a next request can build on it
	// without the caller keeping its own copy. It is a copy: mutating it does not touch the
	// request that was sent.
	Request ChatRequest
	// Response is what the call returned. It can be non-nil alongside Err, because a failover
	// can end with an error after a model has already answered.
	Response *ChatResponse
	// Err is why the attempt was refused: an answer that did not satisfy the contract, or a
	// request that failed. A *StructuredOutputError means the model answered and the answer was
	// unusable; an *AttemptTimeoutError means this attempt's own deadline expired; anything else
	// came from the request, where the provider's retry schedule already did its work.
	Err error
	// Contract is the contract the answer had to satisfy, so the callback can read Schema() or
	// Example() without capturing them.
	Contract *ArgsContract
}

// StructuredAttemptsError reports a structured call that asked for more than one attempt and
// failed. It records how many attempts were spent and unwraps to the last failure, so errors.Is
// and errors.As still reach it. A single-attempt call returns its error unwrapped.
type StructuredAttemptsError struct {
	Attempts uint64
	Err      error
}

func (e *StructuredAttemptsError) Error() string {
	return fmt.Sprintf("structured output used %d attempts: %v", e.Attempts, e.Err)
}

func (e *StructuredAttemptsError) Unwrap() error { return e.Err }

// StructuredNextRequestError reports that the caller's next-request callback failed. The model
// failure that prompted it is kept separately: "my callback broke" and "the model answered badly"
// are different problems, and merging them would leave the caller guessing which one happened.
type StructuredNextRequestError struct {
	Attempt  uint64
	Err      error
	ModelErr error
}

func (e *StructuredNextRequestError) Error() string {
	return fmt.Sprintf("structured output attempt %d: build the next request: %v", e.Attempt, e.Err)
}

func (e *StructuredNextRequestError) Unwrap() error { return e.Err }

// WithStructuredAttempts sets how many structured attempts the call may use. One, the default,
// asks once. A caller that asks for more must also pass WithStructuredNextRequest, because what
// the next request says is the caller's decision and not the framework's.
func WithStructuredAttempts(attempts uint64) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.attempts = attempts
	}
}

// WithStructuredAttemptTimeout bounds one attempt, not one physical request: the deadline covers
// the model call with its failover and transport retries together with the local validation that
// follows, so "two minutes an attempt" cannot stretch into many times that. Zero or negative
// leaves the attempt under the caller's ctx alone, which is the default.
func WithStructuredAttemptTimeout(perAttempt time.Duration) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.attemptTimeout = perAttempt
	}
}

// WithStructuredNextRequest decides what happens after a failed attempt: return a request to try
// it, or nil to stop and report the failure. An error means the callback itself failed, which is
// reported as such rather than as another model failure. The callback is given the caller's ctx,
// never an attempt's expired one, and it is not called when the caller's ctx has ended or when the
// attempts are spent.
func WithStructuredNextRequest(fn func(context.Context, StructuredAttempt) (*ChatRequest, error)) StructuredOption {
	return func(cfg *structuredConfig) {
		cfg.nextRequest = fn
	}
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

	attempts := cfg.attempts
	if attempts == 0 {
		attempts = 1
	}
	if attempts > 1 && cfg.nextRequest == nil {
		return Args{}, nil, errors.New("loom.ChatStructuredArgs: WithStructuredNextRequest is required when WithStructuredAttempts asks for more than one attempt: what the next request says is the caller's decision")
	}

	request := req
	var lastResponse *ChatResponse
	var lastErr error
	for attempt := uint64(1); ; attempt++ {
		args, resp, err := runStructuredAttempt(ctx, cfg, contract, model, purpose, request, schema)
		if err == nil {
			return args, resp, nil
		}
		lastResponse, lastErr = resp, err
		if ctx.Err() != nil {
			// The caller's ctx ended: that error is the story, and the callback is not asked to
			// continue a run the caller stopped.
			return Args{}, lastResponse, lastErr
		}
		if attempt >= attempts {
			return Args{}, lastResponse, wrapStructuredAttempts(attempts, lastErr)
		}
		next, nextErr := cfg.nextRequest(ctx, StructuredAttempt{
			Number:   attempt,
			Request:  cloneChatRequest(request),
			Response: resp,
			Err:      err,
			Contract: contract,
		})
		if nextErr != nil {
			return Args{}, resp, &StructuredNextRequestError{Attempt: attempt, Err: nextErr, ModelErr: err}
		}
		if next == nil {
			return Args{}, lastResponse, wrapStructuredAttempts(attempts, lastErr)
		}
		request = *next
	}
}

// runStructuredAttempt performs one attempt under its own deadline: the model call, with failover
// and transport retries inside it, and the local validation after it.
func runStructuredAttempt(
	ctx context.Context,
	cfg structuredConfig,
	contract *ArgsContract,
	model ChatModel,
	purpose string,
	request ChatRequest,
	schema *Schema,
) (Args, *ChatResponse, error) {
	attemptCtx := ctx
	cancel := func() {}
	if cfg.attemptTimeout > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, cfg.attemptTimeout)
	}
	defer cancel()

	callOptions := append([]CallModelOption{}, cfg.callOptions...)
	callOptions = append(callOptions, withCallModelRequestForModel(func(current ChatModel) (ChatRequest, error) {
		return withStructuredOutputRequest(request, current.Capabilities(), cfg.name, cfg.description, schema)
	}))
	resp, err := CallModel(attemptCtx, purpose, model, request, callOptions...)

	switch {
	case ctx.Err() != nil:
		// While the caller's ctx is live, a deadline inside the attempt belongs to this attempt
		// rather than to the whole call, which is why the two are told apart.
		return Args{}, resp, ctx.Err()
	case err != nil:
		return Args{}, resp, asAttemptTimeout(err, cfg.attemptTimeout)
	case resp.FinishReason == FinishReasonLength:
		return Args{}, resp, &StructuredOutputError{
			Content: resp.Content,
			Err:     fmt.Errorf("finish_reason=%s", resp.FinishReason),
		}
	}
	args, err := contract.DecodeContext(attemptCtx, resp.Content)
	if err == nil && cfg.validate != nil {
		err = cfg.validate(args)
	}
	if err != nil {
		return Args{}, resp, &StructuredOutputError{Content: resp.Content, Err: err}
	}
	return args, resp, nil
}

// asAttemptTimeout names an attempt's own deadline, so a callback can tell it from a failure of
// the request itself.
func asAttemptTimeout(err error, timeout time.Duration) error {
	if timeout <= 0 || err == nil {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &AttemptTimeoutError{Timeout: timeout, Cause: err}
	}
	return err
}

// wrapStructuredAttempts reports how many attempts were spent, but only when the caller asked for
// more than one: a single-attempt call returns exactly what its one call produced.
func wrapStructuredAttempts(allowed uint64, err error) error {
	if allowed <= 1 || err == nil {
		return err
	}
	return &StructuredAttemptsError{Attempts: allowed, Err: err}
}

// cloneChatRequest copies the parts of a request a callback could mutate behind the caller's back.
func cloneChatRequest(request ChatRequest) ChatRequest {
	request.Messages = append([]Message(nil), request.Messages...)
	return request
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
	decoder := stdjson.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return schemaWireValue(out).(map[string]any), nil
}

// schemaWireNumber keeps number tokens numeric even in SDK encoders that treat
// encoding/json.Number as its underlying string kind.
type schemaWireNumber string

func (n schemaWireNumber) MarshalJSON() ([]byte, error) { return []byte(n), nil }

func schemaWireValue(value any) any {
	switch v := value.(type) {
	case stdjson.Number:
		return schemaWireNumber(v)
	case map[string]any:
		for key, item := range v {
			v[key] = schemaWireValue(item)
		}
	case []any:
		for i, item := range v {
			v[i] = schemaWireValue(item)
		}
	}
	return value
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
