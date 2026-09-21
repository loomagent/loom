package loom

import (
	"context"
	"errors"
	"io"
	"strings"
)

// StreamLLMToStep bridges a streaming LLM response to Writer output:
//   - a reasoning_content chunk is emitted to a reasoning item as it arrives
//   - a content chunk accumulates into the returned ChatResponse.Content rather than
//     writing an item, leaving product code to write the final answer or skip it
//   - tool_call chunks are assembled into complete ToolCalls by Index
//
// Reasoning opens lazily:
//   - a model that returns no reasoning_content gets no reasoning item and no empty
//     item
//   - a model that reasons opens the StreamReasoning closure on the first chunk, and
//     flushes whatever accumulated before it
//
// The returned ChatResponse carries Content, ReasoningContent, ToolCalls, Usage,
// FinishReason, and Model. ReasoningContent holds the whole reasoning text, because
// DeepSeek's thinking mode requires the previous assistant message's
// reasoning_content to be passed back unchanged across a multi-turn tool-calling
// loop; otherwise the API reports "The reasoning_content in the thinking mode must be
// passed back to the API". That same reasoning also lands in an item through
// ReasoningStream, so product code only needs to take resp.ReasoningContent into the
// next round for protocol compliance, not to write another item.
//
// A typical ReAct loop in product code:
//
//	for {
//	    resp, err := loom.StreamLLMToStep(ctx, w, "react.main", model, req)
//	    if err != nil { return err }
//	    if len(resp.ToolCalls) == 0 {
//	        return w.FinalAnswer(ctx, resp.Content)
//	    }
//	    results, _ := loom.ExecuteToolCalls(ctx, w, registry, resp.ToolCalls)
//	    // append to msgs for the next round
//	}
func StreamLLMToStep(
	ctx context.Context,
	w Writer,
	purpose string,
	model ChatModel,
	req ChatRequest,
) (*ChatResponse, error) {
	// OTel: start the LLM span, following the GenAI semantic conventions.
	// captureContent, which comes through turnState, decides whether the prompt and the
	// completion are written to span attributes.
	captureContent := false
	if sa, ok := w.(scopeAccessor); ok {
		captureContent = sa.underlyingScope().state.captureContent
	}
	llmCtx, llmSpan := startLLMSpan(ctx, model, req, purpose, captureContent)

	stream, err := model.Stream(llmCtx, req)
	if err != nil {
		finalizeLLMSpan(llmSpan, nil, captureContent, err)
		return nil, err
	}
	defer func() {
		_ = stream.Close()
	}()
	ctx = llmCtx // later reasoning and tool-call writes use this ctx and nest under the LLM span

	var (
		contentBuf        strings.Builder
		bufferedReasoning strings.Builder
		totalReasoning    strings.Builder // the whole reasoning, for the protocol handoff in resp.ReasoningContent
		toolCallsAcc      = map[int]*ToolCall{}
		toolCallsOrder    []int
		usage             *Usage
		finishReason      FinishReason
		modelID           string
	)

	// consume handles one chunk. With rs non-nil, reasoning is emitted live; otherwise
	// it accumulates in the buffer. totalReasoning always accumulates, for the protocol
	// handoff, independently of the rs and buffer branches.
	consume := func(chunk *Chunk, rs ReasoningStream) error {
		if chunk.ReasoningContentDelta != "" {
			totalReasoning.WriteString(chunk.ReasoningContentDelta)
			if rs != nil {
				if err := rs.AppendText(ctx, chunk.ReasoningContentDelta); err != nil {
					return err
				}
			} else {
				bufferedReasoning.WriteString(chunk.ReasoningContentDelta)
			}
		}
		if chunk.ContentDelta != "" {
			contentBuf.WriteString(chunk.ContentDelta)
		}
		for _, d := range chunk.ToolCallDeltas {
			cur, ok := toolCallsAcc[d.Index]
			if !ok {
				cur = &ToolCall{ID: d.ID, Name: d.Name}
				toolCallsAcc[d.Index] = cur
				toolCallsOrder = append(toolCallsOrder, d.Index)
			}
			cur.Arguments += d.Arguments
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.Model != "" {
			modelID = chunk.Model
		}
		return nil
	}

	// Stage 1: consume the stream until the first reasoning chunk or EOF.
	reasoningSeen := false
	for !reasoningSeen {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break // no reasoning at all
		}
		if err != nil {
			return nil, err
		}
		if chunk == nil {
			continue
		}
		if err := consume(chunk, nil); err != nil {
			return nil, err
		}
		if chunk.ReasoningContentDelta != "" {
			reasoningSeen = true
		}
	}

	// Stage 2: open StreamReasoning only when there is reasoning.
	if reasoningSeen {
		rsErr := w.StreamReasoning(ctx, "", func(rs ReasoningStream) error {
			// Flush what stage 1 accumulated, from the first reasoning chunk to here
			if buf := bufferedReasoning.String(); buf != "" {
				if err := rs.AppendText(ctx, buf); err != nil {
					return err
				}
				bufferedReasoning.Reset()
			}
			// Keep consuming the rest of the stream
			for {
				chunk, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if chunk == nil {
					continue
				}
				if err := consume(chunk, rs); err != nil {
					return err
				}
			}
		})
		if rsErr != nil {
			finalizeLLMSpan(llmSpan, nil, captureContent, rsErr)
			return nil, rsErr
		}
	}

	// Stage 3: collect the accumulated tool calls and build the ChatResponse.
	// Note that no tool_call item is written here. ExecuteToolCalls and runOneTool write
	// it just before invoking, which keeps tool_call and tool_result paired and avoids
	// a dangling item when product code decides to skip a call. When the model supplied
	// no ID, one is assigned from the turn's internal counter.
	var calls []ToolCall
	for _, idx := range toolCallsOrder {
		c := toolCallsAcc[idx]
		if c == nil {
			continue // unreachable: order and map are appended together
		}
		if c.ID == "" {
			c.ID = resolveCallID(w)
		}
		calls = append(calls, *c)
	}

	resp := &ChatResponse{
		Content:          contentBuf.String(),
		ReasoningContent: totalReasoning.String(),
		ToolCalls:        calls,
		FinishReason:     finishReason,
		Model:            modelID,
	}
	if usage != nil {
		resp.Usage = *usage
	}

	// Stage 4: emit LLMCalled, so a sink can accumulate usage at the step and turn
	// level. Without a usage frame it is not emitted, which avoids polluting the
	// accumulated fields and an empty UPDATE. The turn root and any step use the same
	// logic: the scope accumulates along the index chain.
	if usage != nil && nonZeroUsage(*usage) {
		if sa, ok := w.(scopeAccessor); ok {
			scope := sa.underlyingScope()
			scope.state.emitLLMCalled(ctx, scope, modelID, purpose, *usage)
		}
	}

	// Stage 5: OTel. Fill the response attributes (usage, model, finish_reason,
	// completion) and end the span.
	finalizeLLMSpan(llmSpan, resp, captureContent, nil)
	return resp, nil
}
