package loom

import (
	"context"
	"errors"
	"io"
	"strings"
)

// StreamLLMToStep 把 LLM 流式输出自动桥接到 Writer 写出:
//   - reasoning_content chunk → 实时 emit 到一条 reasoning item(流式)
//   - content chunk           → 累积到返回值 ChatResponse.Content(不直接写 item;
//     业务方拿到后自决:写 FinalAnswer / 跳过)
//   - tool_call chunks        → 按 Index 拼装成完整 ToolCall,落 WriteToolCall
//
// reasoning lazy 开:
//   - 模型不返 reasoning_content → 不开 reasoning item,不落空 item
//   - 模型返 reasoning → 第一个 reasoning chunk 出现时立即开 StreamReasoning 闭包,
//     之前阶段累积的 reasoning 也立即 flush
//
// 返回 ChatResponse 含 Content / ReasoningContent / ToolCalls / Usage / FinishReason / Model。
// ReasoningContent 累积完整 reasoning 总文本 — DeepSeek thinking 模式要求 multi-turn
// tool calling 时必须把上一轮 assistant message 的 reasoning_content 原样透传给 API
// (否则报 "The reasoning_content in the thinking mode must be passed back to the API")。
// 同一份 reasoning 也通过 ReasoningStream 落到 item,业务方拿 resp.ReasoningContent 拼下一轮
// 是为了协议合规,不需要业务方再次写 item。
//
// 业务方典型 ReAct 循环用法:
//
//	for {
//	    resp, err := loom.StreamLLMToStep(ctx, w, "react.main", model, req)
//	    if err != nil { return err }
//	    if len(resp.ToolCalls) == 0 {
//	        return w.FinalAnswer(ctx, resp.Content)
//	    }
//	    results, _ := loom.ExecuteToolCalls(ctx, w, registry, resp.ToolCalls)
//	    // 拼回 msgs 进入下一轮
//	}
func StreamLLMToStep(
	ctx context.Context,
	w Writer,
	purpose string,
	model ChatModel,
	req ChatRequest,
) (*ChatResponse, error) {
	// OTel:起 LLM span(GenAI semconv)。captureContent 决定 prompt/completion 是否写
	// span attribute(从 turnState 透传)。
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
	ctx = llmCtx // 后续 reasoning stream / tool_call 写入都用这个 ctx,自然嵌入 LLM span

	var (
		contentBuf        strings.Builder
		bufferedReasoning strings.Builder
		totalReasoning    strings.Builder // 累积完整 reasoning,供 resp.ReasoningContent 协议透传
		toolCallsAcc      = map[int]*ToolCall{}
		toolCallsOrder    []int
		usage             *Usage
		finishReason      FinishReason
		modelID           string
	)

	// consume 处理一个 chunk。rs != nil 时 reasoning 实时 emit;否则累积到 buffer。
	// totalReasoning 始终累积(给协议透传用),跟 rs/buffer 分支正交。
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

	// 阶段 1:消费 stream 直到看到第一个 reasoning chunk 或 EOF。
	reasoningSeen := false
	for !reasoningSeen {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break // 全程无 reasoning
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

	// 阶段 2:有 reasoning 才开 StreamReasoning。
	if reasoningSeen {
		rsErr := w.StreamReasoning(ctx, "", func(rs ReasoningStream) error {
			// flush 阶段 1 累积的 reasoning(从看到第一个 reasoning 到这里之间的)
			if buf := bufferedReasoning.String(); buf != "" {
				if err := rs.AppendText(ctx, buf); err != nil {
					return err
				}
				bufferedReasoning.Reset()
			}
			// 继续消费剩余 stream
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

	// 阶段 3:把累积的 tool_calls 收成切片,拼装 ChatResponse。
	// 注意:此处不 WriteToolCall — 让 ExecuteToolCalls/runOneTool 在真正 invoke 前
	// 写 tool_call item,保证 tool_call/tool_result 一一对应,避免提前落 item 后业务方
	// 决定 skip 调用导致悬挂。LLM 没传 ID 时按 turn 内部计数派一个。
	var calls []ToolCall
	for _, idx := range toolCallsOrder {
		c := toolCallsAcc[idx]
		if c == nil {
			continue // 不变量上不会发生:order 跟 map 同时被 append
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

	// 阶段 4:emit LLMCalled — 让 sink 把 usage 累加到 step / turn 维度。
	// 没有 usage 帧时不 emit(避免污染累计字段 + 触发空 UPDATE)。
	// turn 根 / 任意 step 都用同一份逻辑:scope 沿 indices 累加。
	if usage != nil && (usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.TotalTokens > 0) {
		if sa, ok := w.(scopeAccessor); ok {
			scope := sa.underlyingScope()
			scope.state.emitLLMCalled(ctx, scope, modelID, purpose, *usage)
		}
	}

	// 阶段 5:OTel — 填响应 attribute(usage / model / finish_reason / completion),End span。
	finalizeLLMSpan(llmSpan, resp, captureContent, nil)
	return resp, nil
}
