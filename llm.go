package loom

import (
	"context"
	"fmt"
	"slices"
)

// Role is a message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one context message, in plain text. Multimodal input is not
// supported; a later extension will add fields without changing what Content
// means.
type Message struct {
	Role Role

	// source/purpose are runtime-local provenance and are never serialized to a
	// model provider. They distinguish terminal tasks from framework-generated
	// role=user status and retry messages without inspecting content.
	source  MessageSource
	purpose MessagePurpose

	// Content is the main body. For an assistant message from a reasoning model it
	// is the final answer with the reasoning removed. It may be empty for an
	// assistant message that only requested tool calls and produced no text.
	Content string

	// ReasoningContent is the reasoning behind the answer. Only an assistant
	// message may carry it; user, system, and tool messages should leave it empty. A
	// provider that does not support reasoning must ignore it rather than fail.
	ReasoningContent string

	// ToolCalls is carried by assistant messages only: the tool calls this round
	// requested. It replays the previous round's calls back into the history so the
	// model can see which tools it already used.
	ToolCalls []ToolCall

	// Name is optional: the sender's name. Some providers use it to tell several
	// users apart within one context.
	Name string

	// ToolCallID is set on role=tool messages only, linking this result to the call
	// that produced it, matching some ToolCall.ID.
	ToolCallID string
}

// FinishReason is why the model stopped.
type FinishReason string

const (
	FinishReasonStop          FinishReason = "stop"           // stopped naturally
	FinishReasonLength        FinishReason = "length"         // hit max_tokens
	FinishReasonContentFilter FinishReason = "content_filter" // cut off by content moderation
	FinishReasonToolCalls     FinishReason = "tool_calls"     // the model asked to call a tool
	FinishReasonError         FinishReason = "error"          // attributed to a provider failure
)

// Usage is token accounting. The fields mean:
//   - PromptTokens:     input tokens
//   - CompletionTokens: output tokens, reasoning included
//   - CachedTokens:     input tokens that hit the provider's prompt cache, when it
//     reports one
//   - ReasoningTokens:  output tokens that belong to reasoning, on a reasoning model
//   - TotalTokens:      prompt plus completion
type Usage struct {
	PromptTokens     uint64
	CompletionTokens uint64
	CachedTokens     uint64
	ReasoningTokens  uint64
	// ReasoningTokensKnown distinguishes an explicit zero from missing provider telemetry.
	ReasoningTokensKnown bool
	TotalTokens          uint64
}

// ReasoningEffort is a provider-native effort string, not a closed enum.
// ModelCapabilities.ReasoningEfforts supplies the selectable business values.
// Off/alias semantics are provider+model specific; minimal can mean enabled.
type ReasoningEffort string

const (
	ReasoningEffortDefault ReasoningEffort = "" // no effort selected; not a business default
	ReasoningEffortMinimal ReasoningEffort = "minimal"
	ReasoningEffortXHigh   ReasoningEffort = "xhigh"
	ReasoningEffortLow     ReasoningEffort = "low"    // a lower reasoning budget
	ReasoningEffortMedium  ReasoningEffort = "medium" // a medium reasoning budget
	ReasoningEffortHigh    ReasoningEffort = "high"   // a higher reasoning budget
	ReasoningEffortMax     ReasoningEffort = "max"    // the highest reasoning budget
)

// ReasoningMode is the reasoning switch.
//
// It is required: a zero value, "", means the caller declared nothing, and the
// provider fails while building the request. There is deliberately no "follow the
// server default" option, because a provider can flip that default without telling
// anyone. The deepseek-v4 series turns thinking on by server default, which once
// drained a title-summarisation call's max_tokens into reasoning and left the
// content silently empty. Every call site must answer explicitly whether this call
// reasons.
type ReasoningMode string

const (
	ReasoningModeEnabled  ReasoningMode = "enabled"  // reasoning explicitly on
	ReasoningModeDisabled ReasoningMode = "disabled" // reasoning explicitly off
)

// Reasoning controls reasoning. Mode is required; see ReasoningMode.
type Reasoning struct {
	Mode   ReasoningMode
	Effort ReasoningEffort
}

// ResponseFormat controls the output format.
type ResponseFormat string

const (
	ResponseFormatDefault    ResponseFormat = ""            // the provider default, usually text
	ResponseFormatText       ResponseFormat = "text"        // plain text
	ResponseFormatJSONObject ResponseFormat = "json_object" // force JSON output where the provider supports it
)

// StructuredOutputMode describes the structured-output capability, or the level a
// request asks for.
//
// It appears in two places, with slightly different value sets:
//   - ModelCapabilities.StructuredOutput, a capability declaration: None explicitly
//     declares no support, and "" means nothing was declared, so the check passes
//     the request through
//   - ChatRequest.StructuredOutput.Mode, a request: JSONObject or JSONSchema, with
//     "" (Unsupported) meaning no structured output is wanted
type StructuredOutputMode string

const (
	StructuredOutputUnsupported StructuredOutputMode = ""            // request: none wanted / capability: undeclared
	StructuredOutputNone        StructuredOutputMode = "none"        // capability only: explicitly no support
	StructuredOutputJSONObject  StructuredOutputMode = "json_object" // can only ask for a JSON object
	StructuredOutputJSONSchema  StructuredOutputMode = "json_schema" // can take a JSON Schema
)

// ReasoningSupport describes whether reasoning is unsupported, required, or toggleable.
// Provider defaults are diagnostic evidence, not application configuration.
type ReasoningSupport string

const (
	ReasoningSupportNone       ReasoningSupport = "none"
	ReasoningSupportAlwaysOn   ReasoningSupport = "always_on"
	ReasoningSupportToggleable ReasoningSupport = "toggleable"
	// Deprecated: use ReasoningSupportToggleable. Accepted for backwards compatibility.
	ReasoningSupportToggleableDefaultOn ReasoningSupport = "toggleable_default_on"
	// Deprecated: use ReasoningSupportToggleable. Accepted for backwards compatibility.
	ReasoningSupportToggleableDefaultOff ReasoningSupport = "toggleable_default_off"
)

// Canonical returns the three-state capability, normalizing legacy default variants.
func (s ReasoningSupport) Canonical() ReasoningSupport {
	switch s {
	case ReasoningSupportNone, ReasoningSupportAlwaysOn, ReasoningSupportToggleable:
		return s
	case ReasoningSupportToggleableDefaultOn, ReasoningSupportToggleableDefaultOff:
		return ReasoningSupportToggleable
	default:
		return s
	}
}

// ModelCapabilities is what a model declared at initialization.
//
// The caller or modelfactory fills it from the model's real configuration; no
// provider package hardcodes a default. Building a model without Capabilities
// leaves every field at its zero value, meaning nothing was declared, and each
// capability check then passes the request through unchecked.
type ModelCapabilities struct {
	reasoningProbe   bool // only ReasoningProbeCapabilities enables diagnostic bypass
	StructuredOutput StructuredOutputMode

	// Reasoning is the form of reasoning support. A zero value, "", means nothing was
	// declared: ResolveReasoning then only requires Mode and skips the capability
	// cross-check.
	Reasoning ReasoningSupport
	// ReasoningEfforts are the selectable reasoning efforts; empty means the model
	// offers no choice.
	ReasoningEfforts []ReasoningEffort

	// ReasoningEffortsUnconfirmed marks imported, unreviewed metadata. Explicit
	// programmatic capabilities are declarations; database imports must opt out
	// until an administrator confirms them. It never selects a default effort.
	ReasoningEffortsUnconfirmed bool

	// OfficialDefaultReasoningEffort records a reviewed provider default, never a request fallback.
	OfficialDefaultReasoningEffort ReasoningEffort
	// MaxOutputTokens is the output token limit of one call; 0 means unknown.
	MaxOutputTokens uint64
	// MaxContextTokens is the context window limit; 0 means unknown. It is for
	// budgeting and trimming while a request is assembled, not for rejecting one.
	MaxContextTokens uint64
}

// ReasoningSend is the decision to send reasoning parameters, as ResolveReasoning
// returns it, independent of any provider.
type ReasoningSend string

const (
	ReasoningSendOmit     ReasoningSend = "omit"     // send no reasoning switch
	ReasoningSendEnabled  ReasoningSend = "enabled"  // explicitly send "reasoning on"
	ReasoningSendDisabled ReasoningSend = "disabled" // explicitly send "reasoning off"
)

// ResolvedReasoning is the outcome of resolving a reasoning request against the
// model's capabilities. A provider's buildRequest turns it into its own request
// parameters: thinking plus reasoning_effort for deepseek, the Thinking field for
// ark.
type ResolvedReasoning struct {
	Send ReasoningSend
	// Effort is non-empty only when Send is enabled.
	Effort ReasoningEffort
}

// CheckRequestAgainstCapabilities is a defensive check: a request that uses a
// capability the model declared it does not have fails while it is being built and
// is never sent. A contradiction between declaration and use has to surface, not
// be silently downgraded or swallowed.
//
// It covers structured output and output length; reasoning is checked separately by
// ResolveReasoning. An undeclared capability, a zero field, skips its check and
// passes the request through.
//
// Input length against max_context_tokens is deliberately not checked here; do not
// add it. As measured in 2026-06, all three providers (deepseek, ark, openrouter)
// return a 400 before generating when the window is exceeded: no charge, an exact
// token count in the response, and every provider's ErrorClassifier treats a 400 as
// Permanent and does not retry, so the error comes straight back. The provider's own
// rejection is the early warning. A local check would produce false positives,
// because the providers disagree about what counts: deepseek and openrouter
// validate messages plus completion together, while ark looks only at the input and
// happily generates when input plus max_tokens exceeds the window. No single local
// rule can match all three, and a local estimate is never as accurate as the
// provider's tokenizer. max_context_tokens is meant for budgeting, trimming while
// assembling a request and watching Usage.PromptTokens across rounds, not for
// failing a request.
//
// Note the division of labour with ChatStructured: it downgrades deliberately
// according to the declared capability (json_schema → json_object → prompt), so a
// request it builds never exceeds the declaration. This check catches the
// out-of-bounds request written by hand, bypassing ChatStructured.
func CheckRequestAgainstCapabilities(caps ModelCapabilities, req ChatRequest) error {
	if req.StructuredOutput != nil {
		switch req.StructuredOutput.Mode {
		case StructuredOutputJSONSchema:
			switch caps.StructuredOutput {
			case StructuredOutputJSONSchema:
				// supported
			case StructuredOutputUnsupported:
				// capability undeclared; pass through
			case StructuredOutputNone, StructuredOutputJSONObject:
				return fmt.Errorf("loom: model declares structured_output=%q, which does not support json_schema", caps.StructuredOutput)
			default:
				return fmt.Errorf("loom: unknown structured output capability %q", caps.StructuredOutput)
			}
		case StructuredOutputJSONObject:
			switch caps.StructuredOutput {
			case StructuredOutputJSONObject, StructuredOutputJSONSchema:
				// supported
			case StructuredOutputUnsupported:
				// capability undeclared; pass through
			case StructuredOutputNone:
				return fmt.Errorf("loom: model declares structured_output=none, which does not support json_object")
			default:
				return fmt.Errorf("loom: unknown structured output capability %q", caps.StructuredOutput)
			}
		case StructuredOutputUnsupported:
			// the request asks for no structured output, so nothing to check
		case StructuredOutputNone:
			return fmt.Errorf("loom: StructuredOutput.Mode may not be %q; none is reserved for capability declarations", req.StructuredOutput.Mode)
		default:
			return fmt.Errorf("loom: unknown StructuredOutput.Mode %q", req.StructuredOutput.Mode)
		}
	} else if req.ResponseFormat == ResponseFormatJSONObject {
		switch caps.StructuredOutput {
		case StructuredOutputJSONObject, StructuredOutputJSONSchema:
			// supported
		case StructuredOutputUnsupported:
			// capability undeclared; pass through
		case StructuredOutputNone:
			return fmt.Errorf("loom: model declares structured_output=none, which does not support response_format=json_object")
		default:
			return fmt.Errorf("loom: unknown structured output capability %q", caps.StructuredOutput)
		}
	}

	if req.MaxTokens != nil && *req.MaxTokens > 0 &&
		caps.MaxOutputTokens > 0 && uint64(*req.MaxTokens) > caps.MaxOutputTokens {
		return fmt.Errorf("loom: MaxTokens=%d exceeds the output limit of %d the model declared", *req.MaxTokens, caps.MaxOutputTokens)
	}
	return nil
}

// ResolveReasoning resolves the caller's reasoning request against the model's
// capabilities. TestResolveReasoningMatrix in reasoning_test.go covers the whole
// matrix. The essentials:
//   - Mode is required: a zero value fails, which is what forces every call site to
//     decide explicitly
//   - a contradiction between request and capability (none×enabled,
//     always_on×disabled, an unsupported effort) fails rather than being silently
//     ignored
//   - an undeclared capability (caps.Reasoning=="") only requires Mode and passes
//     everything else through
func ResolveReasoning(caps ModelCapabilities, r Reasoning) (ResolvedReasoning, error) {
	if r.Mode == ReasoningModeEnabled && caps.ReasoningEffortsUnconfirmed {
		return ResolvedReasoning{}, fmt.Errorf("loom: an administrator has not confirmed the native reasoning efforts, so reasoning cannot be enabled")
	}

	switch r.Mode {
	case ReasoningModeEnabled, ReasoningModeDisabled:
		// valid; continue
	case "":
		return ResolvedReasoning{}, fmt.Errorf("loom: Reasoning.Mode is required (enabled or disabled); relying on the provider's server-side default is not allowed")
	default:
		return ResolvedReasoning{}, fmt.Errorf("loom: unknown Reasoning.Mode %q", r.Mode)
	}

	if r.Effort != "" && !ValidReasoningEffort(r.Effort) {
		return ResolvedReasoning{}, fmt.Errorf("loom: invalid Reasoning.Effort %q", r.Effort)
	}

	switch r.Mode {
	case ReasoningModeEnabled:
		switch caps.Reasoning {
		case ReasoningSupportNone:
			return ResolvedReasoning{}, fmt.Errorf("loom: the model supports no reasoning (reasoning_support=none), so it cannot be enabled")
		case ReasoningSupportAlwaysOn, ReasoningSupportToggleable,
			ReasoningSupportToggleableDefaultOn,
			ReasoningSupportToggleableDefaultOff,
			"":
			// Explicitly send enabled: harmless and more explicit under always_on, and
			// passed through when the capability is undeclared
		default:
			return ResolvedReasoning{}, fmt.Errorf("loom: unknown reasoning_support %q", caps.Reasoning)
		}
		if r.Effort == ReasoningEffortDefault && len(caps.ReasoningEfforts) > 0 {
			return ResolvedReasoning{}, fmt.Errorf("loom: enabling reasoning requires an explicit effort (supported: %v)", caps.ReasoningEfforts)
		}
		if r.Effort != ReasoningEffortDefault &&
			(caps.Reasoning != "" || caps.ReasoningEfforts != nil) &&
			!slices.Contains(caps.ReasoningEfforts, r.Effort) {
			return ResolvedReasoning{}, fmt.Errorf("loom: the model does not support reasoning effort %q (supported: %v)", r.Effort, caps.ReasoningEfforts)
		}
		return ResolvedReasoning{Send: ReasoningSendEnabled, Effort: r.Effort}, nil

	case ReasoningModeDisabled:
		if r.Effort != ReasoningEffortDefault {
			return ResolvedReasoning{}, fmt.Errorf("loom: Reasoning.Mode=disabled contradicts Effort=%q; a disabled call must not select an effort", r.Effort)
		}
		switch caps.Reasoning {
		case ReasoningSupportNone:
			// the model has no reasoning, so send no parameters
			return ResolvedReasoning{Send: ReasoningSendOmit}, nil
		case ReasoningSupportAlwaysOn:
			return ResolvedReasoning{}, fmt.Errorf("loom: this model cannot turn reasoning off (reasoning_support=always_on)")
		case ReasoningSupportToggleable, ReasoningSupportToggleableDefaultOn,
			ReasoningSupportToggleableDefaultOff,
			"":
			return ResolvedReasoning{Send: ReasoningSendDisabled}, nil
		default:
			return ResolvedReasoning{}, fmt.Errorf("loom: unknown reasoning_support %q", caps.Reasoning)
		}

	default:
		// unreachable: Mode was validated at the top
		return ResolvedReasoning{}, fmt.Errorf("loom: unknown Reasoning.Mode %q", r.Mode)
	}
}

// StructuredOutput is the structured-output constraint of one call.
//
// The absence of a Strict field is deliberate. Strict, a provider's hard guarantee
// that the output conforms, is always what the caller wants; there is no case for
// output that may violate the schema. A provider therefore sends strict=true under
// json_schema and does not make it a switch. As measured in 2026-06, deepseek, ark,
// and openrouter all accept the parameter silently, with no validation pipeline and
// no behavioural difference, so it cannot fail a request. When a provider that
// implements strict the OpenAI way is added, a schema outside the strict subset —
// optional fields, a missing additionalProperties:false — will draw a 400 at request
// time, which is Permanent and not retried; fix the schema.
type StructuredOutput struct {
	Mode        StructuredOutputMode
	Name        string
	Description string
	Schema      *Schema
}

// ChatRequest holds the parameters of one LLM call. Messages is required; a zero
// value in any other field means the provider default.
type ChatRequest struct {
	Messages []Message

	// Tools lists the tools this call may use; nil or empty exposes none. Tool
	// implementations are not part of it: only ToolInfo metadata reaches the model.
	Tools []*ToolInfo

	// ToolChoice is the tool-selection policy; nil means the provider default, which
	// is Auto when Tools are present.
	ToolChoice *ToolChoice

	Temperature *float64
	TopP        *float64
	MaxTokens   *int

	// Stop holds custom stop sequences. One entry becomes the provider's single
	// stop; more than one becomes the stop array.
	Stop []string

	Reasoning      Reasoning
	ResponseFormat ResponseFormat

	// StructuredOutput takes precedence over ResponseFormat. A provider that supports
	// json_schema passes the Schema through unchanged; one that only supports
	// json_object falls back to a JSON object.
	StructuredOutput *StructuredOutput
}

// ChatResponse is the complete result of a synchronous call.
type ChatResponse struct {
	Content          string
	ReasoningContent string

	// ToolCalls holds the calls this round requested, possibly none. The agent runs
	// them with Tool.Invoke and appends the results to the history as role=tool
	// messages for the next round.
	ToolCalls []ToolCall

	FinishReason FinishReason
	Usage        Usage

	// Model is the model id the provider actually used, for auditing and
	// observability. It may differ from the one requested when the provider resolves
	// an alias.
	Model string
}

// Chunk is one increment of a streaming call. Any *Delta field may be an empty
// string, meaning this frame carried nothing for that channel. FinishReason and
// Usage are usually filled in the last frame only.
type Chunk struct {
	ContentDelta          string
	ReasoningContentDelta string

	// ToolCallDeltas are the streaming increments of tool calls, accumulated by Index
	// into complete ToolCalls. The caller keeps that accumulation.
	ToolCallDeltas []ToolCallDelta

	FinishReason FinishReason // set on the last frame; "" in between
	Usage        *Usage       // set on frames carrying usage, usually the last; nil otherwise
	Model        string
}

// Stream is the handle of a streaming call. A typical caller:

// s, err := model.Stream(ctx, req)
// if err != nil { ... }
// defer s.Close()
//
//	for {
//	    ch, err := s.Recv()
//	    if errors.Is(err, io.EOF) { break }
//	    if err != nil { return err }
//	    if ch == nil { continue } // providers occasionally send an empty frame
//	    // handle ch
//	}
type Stream interface {
	// Recv returns the next chunk, or io.EOF once the stream ends naturally. It may
	// return (nil, nil) for an uninformative empty frame, which the caller skips.
	Recv() (*Chunk, error)
	// Close releases the underlying connection. It is idempotent.
	Close() error
}

// ChatModel is one concrete model instance: a provider and a model. See the
// providers/* subpackages for implementations.
type ChatModel interface {
	// Name returns a readable model identifier such as
	// "deepseek/deepseek-v4-flash". It is for logging and observability and takes no
	// part in request routing.
	Name() string

	// Capabilities returns what the model declared at initialization, which the
	// caller uses to decide whether to ask for structured output and the like.
	Capabilities() ModelCapabilities

	// Chat is the synchronous call; it blocks until the complete result arrives.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)

	// Stream is the streaming call. The caller must Close the Stream it returns.
	Stream(ctx context.Context, req ChatRequest) (Stream, error)
}
