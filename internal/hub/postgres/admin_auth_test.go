package postgres

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
)

func TestAdminSessionLifecycle(t *testing.T) {
	ctx, address := newTestSchema(t)
	open := func() *InventoryStore {
		t.Helper()
		store, err := OpenInventory(ctx, address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(store.Close)
		return store
	}
	store := open()
	if _, _, err := store.AdminCredentials(ctx); !errors.Is(err, registry.ErrUnauthorized) {
		t.Fatalf("未初始化管理员时读取凭据: %v", err)
	}
	const firstHash = "fixture-password-hash-1"
	const nextHash = "fixture-password-hash-2"
	if err := store.SetAdminCredentials(ctx, "owner", firstHash); err != nil {
		t.Fatal(err)
	}
	username, hash, err := store.AdminCredentials(ctx)
	if err != nil || username != "owner" || hash != firstHash {
		t.Fatalf("管理员初始化结果不正确: %v", err)
	}
	first := sha256.Sum256([]byte("first session"))
	second := sha256.Sum256([]byte("second session"))
	third := sha256.Sum256([]byte("third session"))
	expires := time.Now().Add(time.Hour)
	if err := store.CreateAdminSession(ctx, firstHash, first, expires); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store = open()
	if err := store.ValidateAdminSession(ctx, first); err != nil {
		t.Fatalf("重新打开数据库后会话失效: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE muster_admin_sessions SET expires_at=statement_timestamp() WHERE token_hash=$1`, first[:]); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateAdminSession(ctx, first); !errors.Is(err, registry.ErrUnauthorized) {
		t.Fatalf("仍接受过期会话: %v", err)
	}
	if err := store.CreateAdminSession(ctx, firstHash, second, expires); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM muster_admin_sessions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("过期会话未清理，行数 %d: %v", count, err)
	}
	for range 2 {
		if err := store.DeleteAdminSession(ctx, second); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ValidateAdminSession(ctx, second); !errors.Is(err, registry.ErrUnauthorized) {
		t.Fatalf("仍接受已退出的会话: %v", err)
	}
	if err := store.CreateAdminSession(ctx, firstHash, third, expires); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAdminCredentials(ctx, "new-owner", nextHash); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateAdminSession(ctx, third); !errors.Is(err, registry.ErrUnauthorized) {
		t.Fatalf("凭据重置后旧会话仍生效: %v", err)
	}
	if err := store.CreateAdminSession(ctx, firstHash, third, expires); !errors.Is(err, registry.ErrUnauthorized) {
		t.Fatalf("旧凭据仍可创建会话: %v", err)
	}
	if err := store.CreateAdminSession(ctx, nextHash, third, expires); err != nil {
		t.Fatalf("更新后的凭据不能创建会话: %v", err)
	}
}
