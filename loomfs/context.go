package loomfs

import (
	"context"
	"errors"
)

type turnSessionKey struct{}

// WithTurnSession 把 turn session 注入 ctx。session 不允许为 nil:
// workspace session 是 executor 运行契约,传 nil 不在此拦截,
// 会在 RequireTurnSession 处报契约错误。
func WithTurnSession(ctx context.Context, session *TurnSession) context.Context {
	return context.WithValue(ctx, turnSessionKey{}, session)
}

func TurnSessionFromContext(ctx context.Context) *TurnSession {
	session, _ := ctx.Value(turnSessionKey{}).(*TurnSession)
	return session
}

// RequireTurnSession 取 ctx 中的 *TurnSession;取不到返回错误。
// workspace session 是 executor 运行契约(dispatcher 必注入),
// executor/工具入口必须经此取 session,不允许 nil 降级。
func RequireTurnSession(ctx context.Context) (*TurnSession, error) {
	session := TurnSessionFromContext(ctx)
	if session == nil {
		return nil, errors.New("loomfs: ctx 缺 TurnSession(workspace session 是 executor 运行契约,dispatcher 必须注入)")
	}
	return session, nil
}
