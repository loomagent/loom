// Package loomfstest 提供测试用的 workspace/session 构造 helper。
// workspace session 是 executor 运行契约,执行 executor/contextpolicy/loomtools
// 相关代码的测试一律经此建真实 session,不允许依赖 nil 降级。
package loomfstest

import (
	"context"
	"testing"

	"github.com/loomagent/loom/loomfs"
)

// NewSession 在 t.TempDir() 建 workspace 并 BeginTurn,返回 session。
// meta 零值字段补默认:ConversationID="conv_test"、TurnIndex=1。
func NewSession(t *testing.T, meta loomfs.TurnMeta) *loomfs.TurnSession {
	t.Helper()
	if meta.ConversationID == "" {
		meta.ConversationID = "conv_test"
	}
	if meta.TurnIndex == 0 {
		meta.TurnIndex = 1
	}
	ws, err := loomfs.OpenWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("loomfstest: OpenWorkspace: %v", err)
	}
	session, err := ws.BeginTurn(meta)
	if err != nil {
		t.Fatalf("loomfstest: BeginTurn: %v", err)
	}
	return session
}

// NewContext 建 session 并注入 ctx,返回 (ctx, session)。
func NewContext(t *testing.T, meta loomfs.TurnMeta) (context.Context, *loomfs.TurnSession) {
	t.Helper()
	session := NewSession(t, meta)
	return loomfs.WithTurnSession(context.Background(), session), session
}
