package loom

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// loom 内核内嵌 OTel 的设计思路:
//
// span 层级(由 ctx 自然嵌套):
//
//	loom.turn                       Run 起的根 span
//	├── loom.step                   Step 闭包起的子 span(可嵌套)
//	│   ├── gen_ai.chat             StreamLLMToStep 起,GenAI semconv
//	│   └── execute_tool            runOneTool 起(ExecuteToolCalls / RunToolByName)
//	└── ...
//
// 全局 tracer = otel.Tracer(tracerName);未 Setup TracerProvider 时返回 noop tracer,
// 业务零开销。也无需在 RunOptions 暴露 Tracer 字段 — 配置走 OTel SDK 全局单例。
//
// captureContent 是 per-Run 决策(可能涉及 PII):
//   - true:prompt / completion / tool args / tool output 落 span attribute
//   - false:只留元数据(model / token / finish_reason / latency)
//
// 不 import semconv 包:gen_ai.* 还在演进,绑死单版 semconv 包反而脆。

// tracerName 进程级唯一标识,用于 instrumentation 库名展示。
const tracerName = "github.com/loomagent/loom"

// OTel attribute keys。GenAI semconv 字面量,OTLP 兼容后端共用此约定。
const (
	// ===== GenAI 通用 =====
	attrGenAISystem        = "gen_ai.system"
	attrGenAIOperationName = "gen_ai.operation.name"

	// ===== LLM 请求 =====
	attrGenAIRequestModel = "gen_ai.request.model"

	// ===== LLM 响应 =====
	attrGenAIResponseModel  = "gen_ai.response.model"
	attrGenAIResponseFinish = "gen_ai.response.finish_reasons"

	// ===== Usage =====
	attrGenAIUsageInputTok  = "gen_ai.usage.input_tokens"
	attrGenAIUsageOutputTok = "gen_ai.usage.output_tokens"
	attrGenAIUsageCachedTok = "gen_ai.usage.cached_tokens"
	attrGenAIUsageReasonTok = "gen_ai.usage.reasoning_tokens"

	// ===== Prompt / Completion(captureContent=true 才写) =====
	attrGenAIPrompt     = "gen_ai.prompt"
	attrGenAICompletion = "gen_ai.completion"

	// ===== Tool =====
	attrGenAIToolName     = "gen_ai.tool.name"
	attrGenAIToolCallID   = "gen_ai.tool.call.id"
	attrGenAIToolArgs     = "gen_ai.tool.call.arguments"
	attrGenAIToolResult   = "gen_ai.tool.call.result"
	attrGenAIToolFinished = "gen_ai.tool.call.status" // "ok" / "failed"

	// ===== loom 自有 =====
	attrLoomTurnIndex      = "loom.turn.index"
	attrLoomTurnPath       = "loom.turn.path"
	attrLoomConversationID = "loom.conversation.id"
	attrLoomStepPath       = "loom.step.path"
	attrLoomStepLabel      = "loom.step.label"
	attrLoomLLMPurpose     = "loom.llm.purpose"
)

// GenAI operation.name 枚举(取自 OTel semconv 建议值)。
const (
	genAIOpChat        = "chat"
	genAIOpExecuteTool = "execute_tool"
)

// genAISystem 把 ChatModel.Name() 翻成 gen_ai.system 值。
// Name() 形如 "deepseek/deepseek-v4-flash" → 取斜杠前作 system,后作 model;
// 不含斜杠时整个串当 system,model 字段留空(provider 实现选择)。
func genAISystem(modelName string) (system, model string) {
	for i := 0; i < len(modelName); i++ {
		if modelName[i] == '/' {
			return modelName[:i], modelName[i+1:]
		}
	}
	return modelName, ""
}

// loomTracer 返回 loom 内核使用的全局 tracer。
// 未 Setup TracerProvider 时返回 noop tracer,所有 Start/End 调用零开销。
func loomTracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// startLLMSpan 起一个 GenAI chat span(StreamLLMToStep 用)。
// 返回带 span 的 ctx,以及 span 句柄(调用方必须 End)。
func startLLMSpan(ctx context.Context, model ChatModel, req ChatRequest, purpose string, captureContent bool) (context.Context, trace.Span) {
	system, modelName := genAISystem(model.Name())
	// span name 用 "{operation} {model}" 形态(GenAI semconv 建议)
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

// finalizeLLMSpan 在 LLM 调用收尾时填响应 attribute + 结束 span。
// err != nil 时 RecordError + Status=Error;否则 Status=Ok。
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

// startToolSpan 起一个 execute_tool span(runOneTool 用)。
// 返回带 span 的 ctx,以及 span 句柄。
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

// finalizeToolSpan 工具收尾。
//   - invokeErr 非 nil:Status=Error + RecordError + tool.call.status="failed"
//   - 否则:Status=Ok + tool.call.status="ok",可选写 output 到 attribute
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

// startTurnSpan 起 Turn 根 span(Run 用)。
// conversation_id 写到 loom.conversation.id,方便后端按对话聚合多轮 trace。
func startTurnSpan(ctx context.Context, st *turnState) (context.Context, trace.Span) {
	ctx, span := loomTracer().Start(ctx, "loom.turn", trace.WithSpanKind(trace.SpanKindInternal))
	span.SetAttributes(
		attribute.Int64(attrLoomTurnIndex, int64(st.turnIdx)),
		attribute.String(attrLoomTurnPath, st.turnPath),
		attribute.String(attrLoomConversationID, st.conversationID),
	)
	// 其它 metadata 透传(user_id / chat_mode_id 等业务自定 K/V)
	for k, v := range st.metadata {
		span.SetAttributes(attribute.String("loom.metadata."+k, v))
	}
	return ctx, span
}

// finalizeTurnSpan Turn 收尾:CloseReason 翻成 span Status。
func finalizeTurnSpan(span trace.Span, st *turnState) {
	// 收尾时 totalUsage 也是 turn 总 token,打到 root span 方便聚合
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
			span.RecordError(fmt.Errorf("queued turn 不应有 close_reason"))
			span.SetStatus(codes.Error, "queued turn 不应有 close_reason")
		case TurnStatusInProgress:
			span.RecordError(fmt.Errorf("in_progress turn 不应有 close_reason"))
			span.SetStatus(codes.Error, "in_progress turn 不应有 close_reason")
		case TurnStatusCompleted:
			span.SetStatus(codes.Ok, "")
		case TurnStatusFailed:
			if st.closeReason.Cause != nil {
				span.RecordError(st.closeReason.Cause)
			}
			span.SetStatus(codes.Error, st.closeReason.Message)
		case TurnStatusCancelled:
			// Cancelled 不算 error(用户主动 / 超时),保持 Unset
		default:
			err := fmt.Errorf("未知 turn close status: %s", status)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}
	span.End()
}

// startStepSpan 起一个 step 子 span(Step 闭包用)。
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

// finalizeStepSpan step 闭包返回时调。
func finalizeStepSpan(span trace.Span, fnErr error) {
	if fnErr != nil {
		span.RecordError(fnErr)
		span.SetStatus(codes.Error, fnErr.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// marshalForSpan 把 prompt / completion / messages 序列化成 JSON 字符串,
// 失败时返 fallback 字符串(不让序列化错误污染整 span)。
func marshalForSpan(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<marshal error: " + err.Error() + ">"
	}
	return string(b)
}

// assistantMessageForSpan 把 ChatResponse 翻成 assistant message 形态供 completion attribute 用。
// 跟 prompt(role + content)对称,方便追踪后端展示对话。
func assistantMessageForSpan(resp *ChatResponse) Message {
	return Message{
		Role:             RoleAssistant,
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		ToolCalls:        resp.ToolCalls,
	}
}
