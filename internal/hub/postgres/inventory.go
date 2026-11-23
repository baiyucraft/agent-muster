// Package postgres 实现 Hub 的 PostgreSQL 持久化。
package postgres

import (
	"context"
	_ "embed"
	"errors"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/im"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/001_device_inventory.sql
var createInventoryTables string

//go:embed migrations/002_workspace.sql
var createWorkspaceTables string

//go:embed migrations/003_m1_workspace.sql
var createM1WorkspaceTables string

//go:embed migrations/004_m1_transfers_and_bindings.sql
var createM1TransferTables string

//go:embed migrations/008_approvals.sql
var createApprovalTables string

//go:embed migrations/009_approval_notifications.sql
var createApprovalNotificationTables string

//go:embed migrations/010_task_controls.sql
var createTaskControlTables string

// InventoryStore 使用 PostgreSQL 保存设备与工作区记录。
type InventoryStore struct {
	pool      *pgxpool.Pool
	recipient im.Recipient
}

// OpenInventory 打开连接池，并在短事务中初始化注册表。
func OpenInventory(ctx context.Context, databaseURL string) (*InventoryStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// 同一数据库同时启动多个 Hub 时，将初始化操作串行化。
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(2047491247, 1)`); err != nil {
			return err
		}
		// 使用 simple protocol 执行完整的内置 SQL 迁移脚本。
		if _, err := tx.Exec(ctx, createInventoryTables, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, createWorkspaceTables, pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, createM1WorkspaceTables, pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, createM1TransferTables, pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			return err
		}
		if err := migrateDeviceRTC(ctx, tx); err != nil {
			return err
		}
		if err := migrateDeviceUpdates(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, createApprovalTables, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, createApprovalNotificationTables, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, createTaskControlTables, pgx.QueryExecModeSimpleProtocol)
		return err
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &InventoryStore{pool: pool}, nil
}

// Close 等待已借出的连接归还，再释放连接池。
func (store *InventoryStore) Close() {
	store.pool.Close()
}

// CreatePairing 写入配对码摘要，并返回数据库计算的过期时间。
func (store *InventoryStore) CreatePairing(ctx context.Context, name string, codeHash [32]byte) (time.Time, error) {
	var expires time.Time
	err := store.pool.QueryRow(ctx, `
		WITH expired AS (
			DELETE FROM muster_pairing_codes WHERE expires_at <= statement_timestamp()
		)
		INSERT INTO muster_pairing_codes (code_hash, device_name, expires_at)
		VALUES ($1, $2, statement_timestamp() + ($3::double precision * interval '1 second'))
		RETURNING expires_at
	`, codeHash[:], name, registry.PairingWindow.Seconds()).Scan(&expires)
	return expires.UTC(), err
}

// Enroll 在同一语句中消费有效配对码并创建设备。
func (store *InventoryStore) Enroll(ctx context.Context, id string, device registry.Enrollment, codeHash, tokenHash [32]byte) error {
	result, err := store.pool.Exec(ctx, `
		WITH claimed AS (
			DELETE FROM muster_pairing_codes
			WHERE code_hash = $1 AND expires_at > clock_timestamp()
			RETURNING device_name
		)
		INSERT INTO muster_devices (device_id, token_hash, name, hostname, os, arch)
		SELECT $2, $3, device_name, $4, $5, $6 FROM claimed
	`, codeHash[:], id, tokenHash[:], device.Hostname, device.OS, device.Arch)
	if err == nil && result.RowsAffected() == 0 {
		return registry.ErrInvalidPairing
	}
	return err
}

// Heartbeat 在写入时核对设备凭据及撤销状态，并更新在线时间。
func (store *InventoryStore) Heartbeat(ctx context.Context, id string, tokenHash [32]byte) error {
	result, err := store.pool.Exec(ctx, `
		UPDATE muster_devices
		SET last_seen_at = clock_timestamp()
		WHERE device_id = $1 AND token_hash = $2 AND revoked_at IS NULL
	`, id, tokenHash[:])
	if err == nil && result.RowsAffected() == 0 {
		return registry.ErrUnauthorized
	}
	return err
}

// Report 在写入时核对凭据，原子替换设备的清单和清单时间。
func (store *InventoryStore) Report(ctx context.Context, id string, tokenHash [32]byte, inventory registry.Inventory) error {
	result, err := store.pool.Exec(ctx, `
		UPDATE muster_devices
		SET inventory = $3, probed_at = clock_timestamp()
		WHERE device_id = $1 AND token_hash = $2 AND revoked_at IS NULL
	`, id, tokenHash[:], inventory)
	if err == nil && result.RowsAffected() == 0 {
		return registry.ErrUnauthorized
	}
	return err
}

// List 读取全部已登记设备，并以当前查询的数据库时间计算在线状态。
func (store *InventoryStore) List(ctx context.Context) ([]registry.Device, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT device_id, name, hostname, os, arch, registered_at, last_seen_at, probed_at, revoked_at, inventory,
		       revoked_at IS NULL AND COALESCE(last_seen_at > statement_timestamp() - ($1::double precision * interval '1 second'), false)
		FROM muster_devices
		ORDER BY device_id
	`, registry.OnlineWindow.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	devices := make([]registry.Device, 0)
	for rows.Next() {
		var device registry.Device
		if err := rows.Scan(
			&device.ID, &device.Name, &device.Hostname, &device.OS, &device.Arch, &device.RegisteredAt,
			&device.LastSeenAt, &device.ProbedAt, &device.RevokedAt, &device.Inventory, &device.Online,
		); err != nil {
			return nil, err
		}
		device.RegisteredAt = device.RegisteredAt.UTC()
		for _, timestamp := range []*time.Time{device.LastSeenAt, device.ProbedAt, device.RevokedAt} {
			if timestamp != nil {
				*timestamp = timestamp.UTC()
			}
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

// Revoke 记录设备首次撤销时间，供列表和后续上报鉴权共同使用。
func (store *InventoryStore) Revoke(ctx context.Context, id string) error {
	var revokedAt time.Time
	err := store.pool.QueryRow(ctx, `
		UPDATE muster_devices SET revoked_at = COALESCE(revoked_at, clock_timestamp())
		WHERE device_id = $1
		RETURNING revoked_at
	`, id).Scan(&revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return registry.ErrNotFound
	}
	return err
}
