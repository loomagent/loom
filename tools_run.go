package loom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ExecuteToolCalls 批量执行 LLM 返回的一组 ToolCall(典型 ReAct 场景)。
//
// 对每个 ToolCall:
//  1. WriteToolCall 写记录(label = call.Name)
//  2. registry.Lookup(call.Name).Invoke(ctx, call.Arguments)
//  3. WriteToolResult 写结果
//
// 第一版串行执行;后续可加并发选项。
//
// 返回结果按入参 calls 顺序排列。任一 Sink 写入失败(strict 模式)返 error 早退;
// 单个 tool 执行失败不中断 — Err 字段填,继续执行下一个。
//
// 业务方典型用法:
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

// ToolExecResult 单个工具执行结果。
type ToolExecResult struct {
	Call   ToolCall // 原始 ToolCall(含 ID/Name/Arguments)
	Output string   // 工具返回值(失败时可能为空)
	Err    error    // tool 未注册 / Invoke 失败时填;Sink 写入失败也会出现在这
}

// RunToolByName 代码编排场景:业务方主动调一个工具,args 是任意 Go 值。
//
// 与 ExecuteToolCalls 区别:
//   - args 是 Go struct/map,内部 json.Marshal,业务方不手拼 JSON
//   - callID 由 loom 自动生成("call_0" / "call_1" /...turn 内递增)
//   - 单调一次,不接受批量
//
// 业务方典型用法:
//
//	type SearchArgs struct{ Query string `json:"query"` }
//	output, err := loom.RunToolByName(ctx, w, "搜索: AI", registry,
//	    "web_search", SearchArgs{Query: "ai"})
func RunToolByName(
	ctx context.Context,
	w Writer,
	label string,
	registry *ToolRegistry,
	name string,
	args any,
) (string, error) {
	argsJSON, err := json.Marshal(args)
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

// runOneTool 内部共用:写 tool_call → invoke → 写 tool_result。
func runOneTool(
	ctx context.Context,
	w Writer,
	label string,
	registry *ToolRegistry,
	call ToolCall,
) (string, error) {
	// 1. 写 tool_call
	if err := w.WriteToolCall(ctx, label, call); err != nil {
		return "", err
	}

	// 从 writer 拿 captureContent 配置(decide tool span 是否落 args/output)
	captureContent := false
	if sa, ok := w.(scopeAccessor); ok {
		captureContent = sa.underlyingScope().state.captureContent
	}

	// 2. 查工具
	tool, ok := registry.Lookup(call.Name)
	if !ok || tool == nil {
		toolErr := &ItemError{
			Code:    "tool_not_found",
			Message: fmt.Sprintf("tool %q not registered", call.Name),
		}
		// OTel:tool 未注册也起 span,标 Error,便于 trace UI 看到这一类失败。
		_, toolSpan := startToolSpan(ctx, call, captureContent)
		finalizeToolSpan(toolSpan, "", captureContent, fmt.Errorf("%s", toolErr.Message))
		_ = w.WriteToolResult(ctx, label, ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Err:      toolErr,
		})
		return "", fmt.Errorf("loom: %s", toolErr.Message)
	}

	// 3. 执行 + 写 tool_result(整个 invoke 全程包在 tool span 内,tool 内部如再起子 span
	//    自然嵌在它之下 — 见 loomtools/* 各 tool 实现)
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

// scopeAccessor helper 用,从 Writer 取出底层 writerScope(进而拿到 turnState)。
// loom 内置 Writer 实现(writerScope / step / turnRoot)都通过 embedding 满足。
type scopeAccessor interface {
	underlyingScope() *writerScope
}

// resolveCallID 为代码编排场景生成 callID。
// 优先用 turnState.nextToolCallIDLocked("call_N");
// 业务方传第三方 Writer 实现时 fallback 到 rand hex。
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
