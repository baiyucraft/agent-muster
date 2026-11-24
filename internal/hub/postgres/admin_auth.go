package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/jackc/pgx/v5"
)

// SetAdminCredentials 初始化或重置唯一的管理员，同时撤销其全部会话。
// passwordHash 是由调用方为本次写入生成的带盐密码哈希。
func (store *InventoryStore) SetAdminCredentials(ctx context.Context, username, passwordHash string) error {
	return pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO muster_admin_credentials (singleton, username, password_hash) VALUES (true, $1, $2)
			ON CONFLICT (singleton) DO UPDATE SET username=EXCLUDED.username, password_hash=EXCLUDED.password_hash
		`, username, passwordHash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM muster_admin_sessions`)
		return err
	})
}

// AdminCredentials 读取唯一管理员的用户名及密码哈希，尚未初始化时返回 registry.ErrUnauthorized。
func (store *InventoryStore) AdminCredentials(ctx context.Context) (username, passwordHash string, err error) {
	err = store.pool.QueryRow(ctx, `SELECT username, password_hash FROM muster_admin_credentials WHERE singleton`).Scan(&username, &passwordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		err = registry.ErrUnauthorized
	}
	return username, passwordHash, err
}

// CreateAdminSession 保存会话令牌摘要及其到期时间，同时清理已过期会话。
// 若管理员凭据已经变更或到期时间已到，返回 registry.ErrUnauthorized。
func (store *InventoryStore) CreateAdminSession(ctx context.Context, passwordHash string, tokenHash [32]byte, expiresAt time.Time) error {
	return pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		// 先获取管理员行的写锁。重置只能在插入前阻止旧凭据，或在插入后删除该会话。
		result, err := tx.Exec(ctx, `
			INSERT INTO muster_admin_sessions (token_hash, expires_at)
			SELECT $2, $3 FROM (
				SELECT singleton FROM muster_admin_credentials WHERE singleton AND password_hash=$1 FOR UPDATE
			) AS credentials WHERE $3 > clock_timestamp()
		`, passwordHash, tokenHash[:], expiresAt)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return registry.ErrUnauthorized
		}
		_, err = tx.Exec(ctx, `DELETE FROM muster_admin_sessions WHERE expires_at <= statement_timestamp()`)
		return err
	})
}

// ValidateAdminSession 用数据库时钟检查会话，摘要不存在或已到期时返回 registry.ErrUnauthorized。
func (store *InventoryStore) ValidateAdminSession(ctx context.Context, tokenHash [32]byte) error {
	var valid bool
	err := store.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM muster_admin_sessions WHERE token_hash=$1 AND expires_at > statement_timestamp())
	`, tokenHash[:]).Scan(&valid)
	if err == nil && !valid {
		return registry.ErrUnauthorized
	}
	return err
}

// DeleteAdminSession 撤销指定会话，重复撤销仍然成功。
func (store *InventoryStore) DeleteAdminSession(ctx context.Context, tokenHash [32]byte) error {
	_, err := store.pool.Exec(ctx, `DELETE FROM muster_admin_sessions WHERE token_hash=$1`, tokenHash[:])
	return err
}
