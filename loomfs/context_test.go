package loomfs

import (
	"context"
	"testing"
)

func TestRequireTurnSession(t *testing.T) {
	ws, err := OpenWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("OpenWorkspace: %v", err)
	}
	session, err := ws.BeginTurn(TurnMeta{ConversationID: "conv_1", TurnIndex: 1})
	if err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}

	ctx := WithTurnSession(context.Background(), session)
	got, err := RequireTurnSession(ctx)
	if err != nil {
		t.Fatalf("RequireTurnSession: %v", err)
	}
	if got != session {
		t.Fatalf("RequireTurnSession 返回的 session 不一致")
	}
}

func TestRequireTurnSessionMissing(t *testing.T) {
	if _, err := RequireTurnSession(context.Background()); err == nil {
		t.Fatalf("缺 session 时 RequireTurnSession 应报错")
	}
	// typed-nil 注入也必须在 Require 处拦下,不允许静默绕过契约。
	ctx := WithTurnSession(context.Background(), nil)
	if _, err := RequireTurnSession(ctx); err == nil {
		t.Fatalf("nil session 注入后 RequireTurnSession 应报错")
	}
}
