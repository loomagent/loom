package loom

import (
	"context"
	"fmt"
	"regexp"
)

const maxToolNameLength = 64

var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidateToolName reports whether name is portable across supported model
// providers. Tool names contain 1 to 64 characters, start with a lowercase
// ASCII letter, and continue with lowercase ASCII letters, digits, or
// underscores. Validation is exact: surrounding whitespace is not trimmed.
func ValidateToolName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if !toolNamePattern.MatchString(name) {
		return fmt.Errorf("name must start with a lowercase ASCII letter and contain only lowercase ASCII letters, digits, or underscores")
	}
	if len(name) > maxToolNameLength {
		return fmt.Errorf("name exceeds %d characters", maxToolNameLength)
	}
	return nil
}

// ToolInfo is a tool's metadata, which the model uses to decide when and how to
// call it.
//
// Name must be unique within a tool set. Description should say clearly when and why
// to use the tool, with a worked example where one helps; it is the single biggest
// influence on how accurately a model calls the tool.
//
// Parameters is a JSON Schema, draft 2020-12 or draft-07. nil means no arguments; an
// empty *Schema{} accepts any JSON.
type ToolInfo struct {
	Name            string
	Description     string
	Parameters      *Schema
	RequiresNetwork bool
	// EndsToolPhase marks a tool whose successful execution ends the tool phase: every later
	// request carries no tools, so that call's content is the final answer by construction. A
	// batch that calls it must call nothing else, and it is never withheld by a tool budget —
	// the phase could not end otherwise.
	EndsToolPhase bool
}

// Tool is one callable tool.
//
// A typical react loop:
//  1. expose the tool metadata, Tool.Info, to the model through ChatRequest.Tools
//  2. the model returns ChatResponse.ToolCalls, or Chunk.ToolCallDeltas to assemble
//  3. find the Tool with ToolRegistry.Lookup(call.Name) and run
//     Tool.Invoke(call.Arguments)
//  4. wrap the returned JSON string in
//     Message{Role: RoleTool, ToolCallID: call.ID, Content: result} and append it to
//     the history for the next round
//
// Loom deliberately ships no high-level node that runs this loop. The agent writes
// the loop; the framework supplies the atomic pieces.
type Tool interface {
	// Info returns the tool's metadata. Repeated calls on one Tool should return
	// logically equivalent results: ctx may affect them, but they should not change
	// often.
	Info(ctx context.Context) (*ToolInfo, error)

	// Invoke runs the tool.
	//
	// argumentsJSON is the argument JSON the model produced. It may be invalid, and
	// the tool validates it. The return value is the tool result fed back to the
	// model as a JSON string of any structure, which the model reads as text.
	//
	// Errors: a tool that fails returns err, and the caller decides whether to feed
	// err.Error() back to the model as the result, letting it choose another tool,
	// retry, or give up. Invoke itself does not turn an error into a result; that is
	// the agent layer's job.
	Invoke(ctx context.Context, argumentsJSON string) (string, error)
}

type invokeFunc func(ctx context.Context, argumentsJSON string) (string, error)

type ToolOption func(*ToolInfo)

// WithEndsToolPhase marks a tool as ending the tool phase. The name is the caller's: the
// framework has no phase enum, and a caller that wants a different name writes a different tool.
func WithEndsToolPhase() ToolOption {
	return func(info *ToolInfo) {
		info.EndsToolPhase = true
	}
}

func WithRequiresNetwork() ToolOption {
	return func(info *ToolInfo) {
		info.RequiresNetwork = true
	}
}

func newTool(name, description string, params *Schema, fn invokeFunc, opts ...ToolOption) Tool {
	info := &ToolInfo{Name: name, Description: description, Parameters: params}
	for _, opt := range opts {
		if opt != nil {
			opt(info)
		}
	}
	return &funcTool{
		info: info,
		fn:   fn,
	}
}

type funcTool struct {
	info *ToolInfo
	fn   invokeFunc
}

func (t *funcTool) Info(context.Context) (*ToolInfo, error) {
	if t.info == nil {
		return nil, nil
	}
	info := *t.info
	if info.Parameters != nil {
		info.Parameters = cloneSchema(info.Parameters)
	}
	return &info, nil
}

func (t *funcTool) Invoke(ctx context.Context, argumentsJSON string) (string, error) {
	return t.fn(ctx, argumentsJSON)
}

// ToolCall is one tool call the model requested, from a non-streaming response or
// assembled from a stream.
type ToolCall struct {
	// ID is the call identity the provider assigned, which pairs the tool result: it
	// becomes the ToolCallID of the role=tool message.
	ID string
	// Name is the tool being called.
	Name string
	// Arguments is the argument JSON the model produced, which is not guaranteed to
	// be valid.
	Arguments string
}

// ToolCallDelta is one increment of a streamed tool call.
//
// A single ToolCall may arrive across several chunks. The first frame usually
// carries Index, ID, and Name plus the first piece of Arguments; later frames carry
// only Index and an Arguments increment to append. Index tells concurrent tool calls
// apart: increments sharing an Index belong to one ToolCall.
type ToolCallDelta struct {
	Index     int
	ID        string // set on the first frame only; later frames are empty
	Name      string // set on the first frame only; later frames are empty
	Arguments string // the increment this frame carries
}

// ToolChoiceMode is the tool-selection policy.
type ToolChoiceMode string

const (
	// ToolChoiceAuto lets the model decide, equivalent to a nil ChatRequest.ToolChoice.
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceNone forbids the model from calling any tool.
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceRequired makes the model call at least one tool.
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceSpecific makes the model call the tool named by ToolChoice.Name.
	ToolChoiceSpecific ToolChoiceMode = "specific"
)

// ToolChoice controls tool selection. It is passed through
// ChatRequest.ToolChoice; nil means the provider default, usually Auto.
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name is used only with Mode=ToolChoiceSpecific, and names the tool that must be
	// called.
	Name string
}

// ToolRegistry manages a set of tools by name, and is usually where the
// ChatRequest.Tools list comes from. It also lets the agent find the Tool to run for
// each entry of ChatResponse.ToolCalls through Lookup.
type ToolRegistry struct {
	tools map[string]Tool
	order []string
}

// NewToolRegistry builds a registry and registers the given tools. It panics on
// failure, so a configuration mistake surfaces at startup.
func NewToolRegistry(tools ...Tool) *ToolRegistry {
	r := &ToolRegistry{tools: map[string]Tool{}}
	for _, t := range tools {
		if err := r.Register(t); err != nil {
			panic(fmt.Sprintf("loom: NewToolRegistry: %v", err))
		}
	}
	return r
}

// Subset selects tools by name and returns a new ToolRegistry, leaving the original
// untouched. An unregistered name is an error, so a configuration mistake surfaces
// at startup or before a call. A typical use declares the full set in the executor
// and lets the handler pick a subset by chat mode.
func (r *ToolRegistry) Subset(names []string) (*ToolRegistry, error) {
	out := NewToolRegistry()
	for _, name := range names {
		t, ok := r.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("loom: tool %q is not in the registry", name)
		}
		if err := out.Register(t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Register adds one tool; a duplicate name is an error. The tool's Name comes from
// Info, which is called once here with context.Background().
func (r *ToolRegistry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("loom: tool must not be nil")
	}
	info, err := t.Info(context.Background())
	if err != nil {
		return fmt.Errorf("loom: Tool.Info failed: %w", err)
	}
	if info == nil {
		return fmt.Errorf("loom: Tool.Info returned nil")
	}
	if err := ValidateToolName(info.Name); err != nil {
		return fmt.Errorf("loom: invalid tool name %q: %w", info.Name, err)
	}
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	if _, exists := r.tools[info.Name]; exists {
		return fmt.Errorf("loom: tool %q is already registered", info.Name)
	}
	r.tools[info.Name] = t
	r.order = append(r.order, info.Name)
	return nil
}

// Lookup finds the tool with the given name.
func (r *ToolRegistry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// InfoList collects every tool's ToolInfo in registration order. An error from any
// Tool.Info stops the walk and is returned.
func (r *ToolRegistry) InfoList(ctx context.Context) ([]*ToolInfo, error) {
	out := make([]*ToolInfo, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		if t == nil {
			continue // unreachable: order and tools are maintained together
		}
		info, err := t.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("loom: tool %q Info: %w", name, err)
		}
		if info == nil {
			return nil, fmt.Errorf("loom: tool %q Info returned nil", name)
		}
		if info.Name != name {
			return nil, fmt.Errorf("loom: tool registered as %q now reports the name %q", name, info.Name)
		}
		out = append(out, info)
	}
	return out, nil
}
