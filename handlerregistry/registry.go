package handlerregistry

import (
	"fmt"
	"sync"

	"github.com/loomagent/loom"
)

// Registry 是并发安全的 loom.Handler 注册表。
//
// 调用方显式 Register,不依赖 import 副作用或 init 顺序:
//
//	registry := handlerregistry.NewRegistry()
//	registry.Register("assistant", assistantHandler)
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]loom.Handler
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]loom.Handler{}}
}

// Register 注册一个 handler。重名或 nil handler 会 panic,用于在启动期暴露配置错误。
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

// Lookup 按 key 查 handler;未注册返回 nil + false。
func (r *Registry) Lookup(key string) (loom.Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[key]
	return h, ok
}

// Keys 返回所有已注册的 key。返回顺序未定义。
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	return out
}
