package sourceregistry

import "context"

type contextKey struct{}

// WithContext attaches registry to ctx. A nil registry leaves ctx unchanged.
func WithContext(ctx context.Context, registry *Registry) context.Context {
	if registry == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, registry)
}

// FromContext returns the attached registry, or nil.
func FromContext(ctx context.Context) *Registry {
	if ctx == nil {
		return nil
	}
	registry, _ := ctx.Value(contextKey{}).(*Registry)
	return registry
}
