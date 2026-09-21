package loom

import (
	"context"
	jsonv2 "encoding/json/v2"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// How OTel is embedded in the loom core:
//
// The span hierarchy, nested naturally through ctx:
//
//	loom.turn                       the root span Run starts
//	├── loom.step                   a child span a Step closure starts, itself nestable
//	│   ├── gen_ai.chat             started by StreamLLMToStep, GenAI semconv
//	│   └── execute_tool            started by runOneTool, for ExecuteToolCalls or RunToolByName
//	└── ...
//
// The global tracer is otel.Tracer(tracerName). Without a configured TracerProvider it
// is the noop tracer, so product code pays nothing. There is deliberately no Tracer
// field on RunOptions; configuration goes through the OTel SDK's global singleton.
//
// captureContent is a per-Run decision, and may involve PII:
//   - true writes the prompt, the completion, tool args, and tool output to span
//     attributes
//   - false keeps only metadata: model, tokens, finish reason, latency
//
// The semconv package is deliberately not imported: gen_ai.* is still moving, and
// binding to one version of semconv would be brittle.

// tracerName identifies this instrumentation library process-wide.
const tracerName = "github.com/loomagent/loom"

// OTel attribute keys. These are GenAI semconv literals, a convention OTLP-compatible
// backends share.
const (
	// ===== GenAI general =====
	attrGenAISystem        = "gen_ai.system"
	attrGenAIOperationName = "gen_ai.operation.name"

	// ===== LLM request =====
	attrGenAIRequestModel = "gen_ai.request.model"

	// ===== LLM response =====
	attrGenAIResponseModel  = "gen_ai.response.model"
	attrGenAIResponseFinish = "gen_ai.response.finish_reasons"

	// ===== Usage =====
	attrGenAIUsageInputTok  = "gen_ai.usage.input_tokens"
	attrGenAIUsageOutputTok = "gen_ai.usage.output_tokens"
	attrGenAIUsageCachedTok = "gen_ai.usage.cached_tokens"
	attrGenAIUsageReasonTok = "gen_ai.usage.reasoning_tokens"

	// ===== Prompt and completion, written only with captureContent=true =====
	attrGenAIPrompt     = "gen_ai.prompt"
	attrGenAICompletion = "gen_ai.completion"

	// ===== Tool =====
	attrGenAIToolName     = "gen_ai.tool.name"
	attrGenAIToolCallID   = "gen_ai.tool.call.id"
	attrGenAIToolArgs     = "gen_ai.tool.call.arguments"
	attrGenAIToolResult   = "gen_ai.tool.call.result"
	attrGenAIToolFinished = "gen_ai.tool.call.status" // "ok" / "failed"

	// ===== loom's own =====
	attrLoomTurnIndex      = "loom.turn.index"
	attrLoomTurnPath       = "loom.turn.path"
	attrLoomConversationID = "loom.conversation.id"
	attrLoomStepPath       = "loom.step.path"
	attrLoomStepLabel      = "loom.step.label"
	attrLoomLLMPurpose     = "loom.llm.purpose"
)

// The GenAI operation.name values, taken from the OTel semconv recommendations.
const (
	genAIOpChat        = "chat"
	genAIOpExecuteTool = "execute_tool"
)

// genAISystem turns ChatModel.Name() into a gen_ai.system value. A name such as
// "deepseek/deepseek-v4-flash" gives the part before the slash as system and the part
// after as model. Without a slash the whole string is the system and the model field
// stays empty; individual providers may decide otherwise.
func genAISystem(modelName string) (system, model string) {
	for i := 0; i < len(modelName); i++ {
		if modelName[i] == '/' {
			return modelName[:i], modelName[i+1:]
		}
	}
	return modelName, ""
}

// loomTracer returns the global tracer the loom core uses. Without a configured
// TracerProvider it is the noop tracer, so every Start and End costs nothing.
func loomTracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// startLLMSpan starts a GenAI chat span for StreamLLMToStep. It returns a ctx carrying
// the span and the span itself, which the caller must End.
func startLLMSpan(ctx context.Context, model ChatModel, req ChatRequest, purpose string, captureContent bool) (context.Context, trace.Span) {
	system, modelName := genAISystem(model.Name())
	// The span name follows the "{operation} {model}" shape the GenAI semconv suggests
	name := genAIOpChat
	if modelName != "" {
		name = genAIOpChat + " " + modelName
	} else if system != "" {
		name = genAIOpChat + " " + system
	}
	ctx, span := loomTracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient))
	span.SetAttributes(
		attribute.String(attrGenAIOperationName, genAIOpChat),
		attribute.String(attrGenAISystem, system),
		attribute.String(attrGenAIRequestModel, modelName),
	)
	if purpose != "" {
		span.SetAttributes(attribute.String(attrLoomLLMPurpose, purpose))
	}
	if captureContent && len(req.Messages) > 0 {
		span.SetAttributes(attribute.String(attrGenAIPrompt, marshalForSpan(req.Messages)))
	}
	return ctx, span
}

// finalizeLLMSpan fills the response attributes and ends the span when the call
// returns. A non-nil err records the error and sets Status=Error; otherwise Status is
// Ok.
func finalizeLLMSpan(span trace.Span, resp *ChatResponse, captureContent bool, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return
	}
	if resp != nil {
		attrs := []attribute.KeyValue{
			attribute.String(attrGenAIResponseModel, resp.Model),
			attribute.Int64(attrGenAIUsageInputTok, int64(resp.Usage.PromptTokens)),
			attribute.Int64(attrGenAIUsageOutputTok, int64(resp.Usage.CompletionTokens)),
			attribute.Int64(attrGenAIUsageCachedTok, int64(resp.Usage.CachedTokens)),
			attribute.Int64(attrGenAIUsageReasonTok, int64(resp.Usage.ReasoningTokens)),
		}
		if resp.FinishReason != "" {
			attrs = append(attrs, attribute.StringSlice(attrGenAIResponseFinish, []string{string(resp.FinishReason)}))
		}
		span.SetAttributes(attrs...)
		if captureContent {
			completion := assistantMessageForSpan(resp)
			span.SetAttributes(attribute.String(attrGenAICompletion, marshalForSpan(completion)))
		}
	}
	span.SetStatus(codes.Ok, "")
	span.End()
}

// startToolSpan starts an execute_tool span for runOneTool, returning a ctx carrying
// the span and the span itself.
func startToolSpan(ctx context.Context, call ToolCall, captureContent bool) (context.Context, trace.Span) {
	name := genAIOpExecuteTool + " " + call.Name
	ctx, span := loomTracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindInternal))
	span.SetAttributes(
		attribute.String(attrGenAIOperationName, genAIOpExecuteTool),
		attribute.String(attrGenAIToolName, call.Name),
		attribute.String(attrGenAIToolCallID, call.ID),
	)
	if captureContent && call.Arguments != "" {
		span.SetAttributes(attribute.String(attrGenAIToolArgs, call.Arguments))
	}
	return ctx, span
}

// finalizeToolSpan closes a tool. A non-nil invokeErr sets Status=Error, records the
// error, and marks tool.call.status="failed"; otherwise Status is Ok with
// tool.call.status="ok", and the output may be written to an attribute.
func finalizeToolSpan(span trace.Span, output string, captureContent bool, invokeErr error) {
	if invokeErr != nil {
		span.SetAttributes(attribute.String(attrGenAIToolFinished, "failed"))
		span.RecordError(invokeErr)
		span.SetStatus(codes.Error, invokeErr.Error())
		span.End()
		return
	}
	span.SetAttributes(attribute.String(attrGenAIToolFinished, "ok"))
	if captureContent && output != "" {
		span.SetAttributes(attribute.String(attrGenAIToolResult, output))
	}
	span.SetStatus(codes.Ok, "")
	span.End()
}

// startTurnSpan starts the Turn root span for Run. The conversation id goes to
// loom.conversation.id, which lets a backend aggregate the traces of one conversation.
func startTurnSpan(ctx context.Context, st *turnState) (context.Context, trace.Span) {
	ctx, span := loomTracer().Start(ctx, "loom.turn", trace.WithSpanKind(trace.SpanKindInternal))
	span.SetAttributes(
		attribute.Int64(attrLoomTurnIndex, int64(st.turnIdx)),
		attribute.String(attrLoomTurnPath, st.turnPath),
		attribute.String(attrLoomConversationID, st.conversationID),
	)
	// Other metadata passes through: user_id, chat_mode_id, and whatever else product
	// code defines
	for k, v := range st.metadata {
		span.SetAttributes(attribute.String("loom.metadata."+k, v))
	}
	return ctx, span
}

// finalizeTurnSpan closes a Turn, turning CloseReason into the span Status.
func finalizeTurnSpan(span trace.Span, st *turnState) {
	// At close time totalUsage is the turn's total tokens; putting it on the root span
	// makes aggregation easy
	span.SetAttributes(
		attribute.Int64(attrGenAIUsageInputTok, int64(st.totalUsage.PromptTokens)),
		attribute.Int64(attrGenAIUsageOutputTok, int64(st.totalUsage.CompletionTokens)),
		attribute.Int64(attrGenAIUsageCachedTok, int64(st.totalUsage.CachedTokens)),
		attribute.Int64(attrGenAIUsageReasonTok, int64(st.totalUsage.ReasoningTokens)),
	)
	if st.closeReason != nil {
		status := statusFromCloseReason(st.closeReason)
		span.SetAttributes(
			attribute.String("loom.close.status", string(status)),
			attribute.String("loom.close.code", string(st.closeReason.Code)),
		)
		switch status {
		case TurnStatusQueued:
			span.RecordError(fmt.Errorf("a queued turn must not carry a close_reason"))
			span.SetStatus(codes.Error, "a queued turn must not carry a close_reason")
		case TurnStatusInProgress:
			span.RecordError(fmt.Errorf("an in_progress turn must not carry a close_reason"))
			span.SetStatus(codes.Error, "an in_progress turn must not carry a close_reason")
		case TurnStatusCompleted:
			span.SetStatus(codes.Ok, "")
		case TurnStatusFailed:
			if st.closeReason.Cause != nil {
				span.RecordError(st.closeReason.Cause)
			}
			span.SetStatus(codes.Error, st.closeReason.Message)
		case TurnStatusCancelled:
			// Cancelled is not an error, whether the user asked or it timed out, so it
			// stays Unset
		default:
			err := fmt.Errorf("unknown turn close status: %s", status)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}
	span.End()
}

// startStepSpan starts a child step span for a Step closure.
func startStepSpan(ctx context.Context, path, label string) (context.Context, trace.Span) {
	name := "loom.step"
	if label != "" {
		name = "loom.step " + label
	}
	ctx, span := loomTracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindInternal))
	span.SetAttributes(
		attribute.String(attrLoomStepPath, path),
		attribute.String(attrLoomStepLabel, label),
	)
	return ctx, span
}

// finalizeStepSpan is called when a Step closure returns.
func finalizeStepSpan(span trace.Span, fnErr error) {
	if fnErr != nil {
		span.RecordError(fnErr)
		span.SetStatus(codes.Error, fnErr.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// marshalForSpan serializes a prompt, completion, or message list into a JSON string,
// falling back to a placeholder on failure so a serialization error cannot spoil the
// span.
func marshalForSpan(v any) string {
	b, err := jsonv2.Marshal(v)
	if err != nil {
		return "<marshal error: " + err.Error() + ">"
	}
	return string(b)
}

// assistantMessageForSpan turns a ChatResponse into an assistant message for the
// completion attribute. It mirrors the prompt, role plus content, which makes a
// conversation easy to read in a tracing backend.
func assistantMessageForSpan(resp *ChatResponse) Message {
	return Message{
		Role:             RoleAssistant,
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		ToolCalls:        resp.ToolCalls,
	}
}
