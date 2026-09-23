package loom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
)

// ExecuteToolCalls runs the set of tool calls a model returned, the typical ReAct
// case. For every call it writes the tool_call, looks the tool up and invokes it,
// then writes the tool_result.
//
// This first version runs serially.
//
// The results come back in the order of the calls argument. A failed sink write, in
// strict mode, returns early with an error; one tool failing does not stop the rest,
// it fills in Err and the next call runs.
//
// A typical use:
//
//	results, err := loom.ExecuteToolCalls(ctx, w, registry, resp.ToolCalls)
//	if err != nil { return err }
//	for _, r := range results {
//	    msgs = append(msgs, loom.Message{Role: loom.RoleTool, ToolCallID: r.Call.ID, Content: r.Output})
//	}
func ExecuteToolCalls(
	ctx context.Context,
	w Writer,
	registry *ToolRegistry,
	calls []ToolCall,
) ([]ToolExecResult, error) {
	if w == nil {
		return nil, errors.New("loom.ExecuteToolCalls: writer must not be nil")
	}
	if registry == nil {
		return nil, errors.New("loom.ExecuteToolCalls: registry must not be nil")
	}
	out := make([]ToolExecResult, 0, len(calls))
	for _, call := range calls {
		output, err := runOneTool(ctx, w, call.Name, registry, call)
		out = append(out, ToolExecResult{
			Call:   call,
			Output: output,
			Err:    err,
		})
	}
	return out, nil
}

// ToolExecResult is the outcome of running one tool.
type ToolExecResult struct {
	Call   ToolCall // the original ToolCall, with its ID, name, and arguments
	Output string   // the tool's return value, possibly empty on failure
	Err    error    // set when the tool is unregistered or Invoke fails; a sink write failure lands here too
}

// RunToolByName is the code-orchestration case: product code calls one tool itself,
// with args as any Go value.
//
// How it differs from ExecuteToolCalls:
//   - args is a Go struct or map that jsonv2.Marshal handles here, so product code
//     never assembles JSON by hand
//   - loom assigns the call ID ("call_0", "call_1", ...), incrementing within the turn
//   - one call at a time, no batches
//
// A typical use:
//
//	type SearchArgs struct{ Query string `json:"query"` }
//	output, err := loom.RunToolByName(ctx, w, "search: AI", registry,
//	    "web_search", SearchArgs{Query: "ai"})
func RunToolByName(
	ctx context.Context,
	w Writer,
	label string,
	registry *ToolRegistry,
	name string,
	args any,
) (string, error) {
	if w == nil {
		return "", errors.New("loom.RunToolByName: writer must not be nil")
	}
	if registry == nil {
		return "", errors.New("loom.RunToolByName: registry must not be nil")
	}
	argsJSON, err := jsonv2.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("loom.RunToolByName: marshal args: %w", err)
	}
	call := ToolCall{
		ID:        resolveCallID(w),
		Name:      name,
		Arguments: string(argsJSON),
	}
	return runOneTool(ctx, w, label, registry, call)
}

// runOneTool is shared internally: write the tool_call, invoke, write the tool_result.
func runOneTool(
	ctx context.Context,
	w Writer,
	label string,
	registry *ToolRegistry,
	call ToolCall,
) (string, error) {
	// 1. write the tool_call
	if err := w.WriteToolCall(ctx, label, call); err != nil {
		return "", err
	}

	// Take captureContent from the writer, which decides whether the tool span records
	// args and output
	captureContent := false
	if sa, ok := w.(scopeAccessor); ok {
		captureContent = sa.underlyingScope().state.captureContent
	}

	// 2. look the tool up
	tool, ok := registry.Lookup(call.Name)
	if !ok || tool == nil {
		toolErr := &ItemError{
			Code:    "tool_not_found",
			Message: fmt.Sprintf("tool %q not registered", call.Name),
		}
		// OTel: an unregistered tool still gets a span, marked Error, so a trace UI can
		// show this class of failure
		_, toolSpan := startToolSpan(ctx, call, captureContent)
		finalizeToolSpan(toolSpan, "", captureContent, fmt.Errorf("%s", toolErr.Message))
		_ = w.WriteToolResult(ctx, label, ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Err:      toolErr,
		})
		return "", fmt.Errorf("loom: %s", toolErr.Message)
	}

	// 3. invoke, then write the tool_result. The whole invoke runs inside the tool
	// span, so any child span a tool starts nests below it; see the loomtools/*
	// implementations
	toolCtx, toolSpan := startToolSpan(ctx, call, captureContent)
	output, invokeErr := tool.Invoke(toolCtx, call.Arguments)
	finalizeToolSpan(toolSpan, output, captureContent, invokeErr)

	result := ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Output:   output,
	}
	if invokeErr != nil {
		result.Err = &ItemError{
			Code:    "tool_failed",
			Message: invokeErr.Error(),
		}
	}
	if err := w.WriteToolResult(ctx, label, result); err != nil {
		return output, err
	}
	return output, invokeErr
}

// scopeAccessor is what a helper uses to reach the writerScope behind a Writer, and
// from it the turnState. Every built-in Writer in loom (writerScope, step, turnRoot)
// satisfies it through embedding.
type scopeAccessor interface {
	underlyingScope() *writerScope
}

// resolveCallID builds a call ID for the code-orchestration case. It prefers
// turnState.nextToolCallIDLocked, "call_N", and falls back to random hex when product
// code passes a third-party Writer implementation.
func resolveCallID(w Writer) string {
	if sa, ok := w.(scopeAccessor); ok {
		s := sa.underlyingScope().state
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.nextToolCallIDLocked()
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}
