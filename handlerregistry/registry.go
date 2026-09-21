package handlerregistry

import (
	"fmt"
	"sync"

	"github.com/loomagent/loom"
)

// Registry is a registry of loom.Handlers, safe for concurrent use.
//
// Callers register explicitly, with no reliance on import side effects or init order:
//
//	registry := handlerregistry.NewRegistry()
//	registry.Register("assistant", assistantHandler)
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]loom.Handler
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]loom.Handler{}}
}

// Register adds one handler. A duplicate key or a nil handler panics, which surfaces a
// configuration mistake at startup.
func (r *Registry) Register(key string, h loom.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[key]; exists {
		panic(fmt.Sprintf("handlerregistry: handler %q already registered", key))
	}
	if h == nil {
		panic(fmt.Sprintf("handlerregistry: handler %q cannot be nil", key))
	}
	r.handlers[key] = h
}

// Lookup finds a handler by key; an unregistered key returns nil and false.
func (r *Registry) Lookup(key string) (loom.Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[key]
	return h, ok
}

// Keys returns every registered key, in no particular order.
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	return out
}
