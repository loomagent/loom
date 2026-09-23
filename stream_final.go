package loom

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrToolCallInFinalAnswer reports a tool call in a response that must contain
// only reasoning and answer text. The call is never executed.
var ErrToolCallInFinalAnswer = errors.New("loom: the model called a tool in the final answer")

// ErrEmptyFinalAnswer reports a normally terminated response with no answer text.
var ErrEmptyFinalAnswer = errors.New("loom: the model returned an empty final answer")

// StreamLLMToFinalAnswer streams optional reasoning and answer text to separate
// items. Each nonempty delta reaches the Writer before the next frame is read.
// Reasoning opens only when present; the final_answer opens at its first content
// delta. A successful return commits the Turn, so the caller must not subsequently
// call FinalAnswer. The request must contain no tools.
//
// Normal stop, nonempty content, and successful stream consumption are required
// before committing. On error the partial items remain available, but the answer
// is not committed. Use RunOptions.StrictSink for mandatory persistence.
// This helper does not retry a partially delivered answer.
func StreamLLMToFinalAnswer(ctx context.Context, w TurnWriter, purpose string, model ChatModel, req ChatRequest) (response *ChatResponse, retErr error) {
	if w == nil || model == nil {
		return nil, errors.New("loom.StreamLLMToFinalAnswer: writer and model are required")
	}
	if len(req.Tools) != 0 || (req.ToolChoice != nil && req.ToolChoice.Mode != ToolChoiceNone) {
		return nil, errors.New("loom.StreamLLMToFinalAnswer: request must disable tools")
	}
	provider, modelName := SplitModelName(model.Name())
	if _, err := ResolveModelReasoning(provider, modelName, model.Capabilities(), req.Reasoning); err != nil {
		return nil, err
	}
	if err := finalStreamWriteError(ctx, w); err != nil {
		return nil, err
	}
	captureContent := false
	if sa, ok := w.(scopeAccessor); ok {
		state := sa.underlyingScope().state
		state.mu.Lock()
		closed, inProgress := state.isClosed(), state.finalAnswerInProgress
		state.mu.Unlock()
		if closed {
			return nil, ErrTurnClosed
		}
		if inProgress {
			return nil, ErrFinalAnswerInProgress
		}
		captureContent = state.captureContent
	}
	ctx, span := startLLMSpan(ctx, model, req, purpose, captureContent)
	defer func() { finalizeLLMSpan(span, response, captureContent, retErr) }()
	stream, err := model.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	response = &ChatResponse{}
	var content, reasoning strings.Builder
	var details []jsontext.Value
	var usage *Usage
	usageReported := false
	// Settle usage before the final answer commits. The deferred call also covers
	// failures while opening or consuming a stream, without counting usage twice.
	settle := func() {
		response.Content = content.String()
		response.ReasoningContent = reasoning.String()
		response.ReasoningDetails = joinJSONElements(details)
		if usage != nil {
			response.Usage = *usage
			if !usageReported && nonZeroUsage(*usage) {
				usageReported = true
				if sa, ok := w.(scopeAccessor); ok {
					scope := sa.underlyingScope()
					scope.state.emitLLMCalled(ctx, scope, response.Model, purpose, *usage)
				}
			}
		}
	}
	defer settle()

	var pending *Chunk
	var firstContent string
	var answer FinalAnswerStream
	var consume func(ReasoningStream) error
	consume = func(rs ReasoningStream) error {
		for {
			if err := finalStreamWriteError(ctx, w); err != nil {
				return err
			}
			chunk := pending
			pending = nil
			if chunk == nil {
				var err error
				chunk, err = stream.Recv()
				if errors.Is(err, io.EOF) {
					return finalStreamWriteError(ctx, w)
				}
				if err != nil {
					return err
				}
				if chunk == nil {
					continue
				}
			}
			if chunk.ReasoningContentDelta != "" && rs == nil {
				pending = chunk
				return w.StreamReasoning(ctx, "", consume)
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			if chunk.FinishReason != "" {
				response.FinishReason = chunk.FinishReason
			}
			if chunk.Model != "" {
				response.Model = chunk.Model
			}
			if len(chunk.ToolCallDeltas) > 0 {
				return ErrToolCallInFinalAnswer
			}
			if len(chunk.ReasoningDetails) > 0 {
				var elements []jsontext.Value
				if err := jsonv2.Unmarshal(chunk.ReasoningDetails, &elements); err == nil {
					details = append(details, elements...)
				}
			}
			if chunk.ReasoningContentDelta != "" {
				reasoning.WriteString(chunk.ReasoningContentDelta)
				if err := rs.AppendText(ctx, chunk.ReasoningContentDelta); err != nil {
					return err
				}
			}
			if chunk.ContentDelta != "" {
				content.WriteString(chunk.ContentDelta)
				if answer == nil {
					// Close the initial reasoning item before opening the answer.
					// Only this single content frame is held, never the whole answer.
					firstContent = chunk.ContentDelta
					return nil
				}
				if err := answer.AppendText(ctx, chunk.ContentDelta); err != nil {
					return err
				}
			}
		}
	}
	finish := func() error {
		settle()
		if err := finalStreamWriteError(ctx, w); err != nil {
			return err
		}
		return validateFinalAnswer(response)
	}
	if err := consume(nil); err != nil {
		return response, err
	}
	if firstContent == "" {
		return response, finish()
	}
	err = w.StreamFinalAnswer(ctx, func(fs FinalAnswerStream) error {
		answer = fs
		if err := finalStreamWriteError(ctx, w); err != nil {
			return err
		}
		if err := fs.AppendText(ctx, firstContent); err != nil {
			return err
		}
		if err := consume(nil); err != nil {
			return err
		}
		return finish()
	})
	if err == nil {
		err = finalStreamWriteError(ctx, w)
	}
	return response, err
}

func validateFinalAnswer(response *ChatResponse) error {
	switch response.FinishReason {
	case FinishReasonStop:
	case FinishReasonContentFilter:
		return ErrContentFilter
	case FinishReasonLength:
		return ErrOutputTruncated
	case FinishReasonToolCalls:
		return ErrToolCallInFinalAnswer
	default:
		return fmt.Errorf("loom: final answer requires a normal stop, got %q", response.FinishReason)
	}
	if strings.TrimSpace(response.Content) == "" {
		return ErrEmptyFinalAnswer
	}
	return nil
}

// Strict sinks record errors in turnState. Observe them while consuming the
// model, so a failed delta stops the stream and cannot commit a partial answer.
func finalStreamWriteError(ctx context.Context, w Writer) error {
	if sa, ok := w.(scopeAccessor); ok {
		state := sa.underlyingScope().state
		state.mu.Lock()
		err := state.sinkErr
		state.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
