package repo

import (
	"context"
	"fmt"
	"time"
)

// GetBridgeToken 读取未过期的桥接授权 token（过期视为不存在），供 bridge.TokenCache 使用。
// 持久化到 PG：容器重建后 token 缓存仍存活（TTL 内免重复授权）。
func (r *Repo) GetBridgeToken(ctx context.Context, userID string, now time.Time) (string, time.Time, bool) {
	if userID == "" {
		return "", time.Time{}, false
	}
	var token string
	var expiresAt time.Time
	if err := r.pool.QueryRow(ctx,
		`SELECT token, expires_at FROM bridge_tokens WHERE user_id = $1`, userID).
		Scan(&token, &expiresAt); err != nil {
		return "", time.Time{}, false
	}
	if now.After(expiresAt) {
		return "", time.Time{}, false
	}
	return token, expiresAt, true
}

// PutBridgeToken 幂等写入（upsert）桥接授权 token。
func (r *Repo) PutBridgeToken(ctx context.Context, userID, token string, expiresAt time.Time) error {
	if userID == "" || token == "" {
		return fmt.Errorf("bridge token: user/token required")
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO bridge_tokens (user_id, token, expires_at)
		VALUES ($1,$2,$3)
		ON CONFLICT (user_id) DO UPDATE
		SET token = EXCLUDED.token, expires_at = EXCLUDED.expires_at, created_at = now()`,
		userID, token, expiresAt)
	return err
}
