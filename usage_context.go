package loom

import "context"

type usageScopeContextKey struct{}

// withUsageScope binds the current writer scope to the call context. This lets
// synchronous model helpers report usage without adding a Writer parameter to
// every business-layer function. The value is private to loom and never crosses
// process or persistence boundaries.
func withUsageScope(ctx context.Context, scope *writerScope) context.Context {
	if ctx == nil || scope == nil {
		return ctx
	}
	return context.WithValue(ctx, usageScopeContextKey{}, scope)
}

func usageScopeFromContext(ctx context.Context) *writerScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(usageScopeContextKey{}).(*writerScope)
	return scope
}

func nonZeroUsage(usage Usage) bool {
	return usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.CachedTokens > 0 || usage.ReasoningTokens > 0 || usage.TotalTokens > 0
}
