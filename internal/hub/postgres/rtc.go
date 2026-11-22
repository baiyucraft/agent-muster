package postgres

import (
	"bytes"
	"context"
	_ "embed"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/peer"
	"github.com/jackc/pgx/v5"
)

//go:embed migrations/005_m1_rtc.sql
var createRTCTables string

//go:embed migrations/007_rtc_public_keys.sql
var createRTCPublicKeys string

func migrateDeviceRTC(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, createRTCTables, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, createRTCPublicKeys, pgx.QueryExecModeSimpleProtocol)
	return err
}

// SaveRTCPublicKey 保存指定验收投递参与设备的第一份公钥。重复声明相同公钥可重试。
func (store *InventoryStore) SaveRTCPublicKey(ctx context.Context, device string, token [32]byte, transfer string, key []byte) error {
	return pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM muster_transfers
			WHERE id=$1 AND $2 IN (source_device_id,target_device_id))`, transfer, device).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return notFound(pgx.ErrNoRows)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO muster_rtc_public_keys(device_id,public_key) VALUES($1,$2)
			ON CONFLICT(device_id) DO NOTHING`, device, key); err != nil {
			return err
		}
		var saved []byte
		if err := tx.QueryRow(ctx, `SELECT public_key FROM muster_rtc_public_keys WHERE device_id=$1`, device).Scan(&saved); err != nil {
			return err
		}
		if !bytes.Equal(key, saved) {
			return workspace.ErrConflict
		}
		return nil
	})
}

// RegisterRTC 将已认证设备的信令可用期续到 90 秒，并清理已过期的信令。
func (store *InventoryStore) RegisterRTC(ctx context.Context, device string, token [32]byte) error {
	return pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM muster_rtc_signals WHERE expires_at <= statement_timestamp()"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO muster_rtc_devices(device_id,expires_at) VALUES($1,statement_timestamp()+interval '90 seconds')
			ON CONFLICT(device_id) DO UPDATE SET expires_at=EXCLUDED.expires_at`, device)
		return err
	})
}

// RTCOffers 返回指定收件设备尚未应答的有效信令。已收妥的投递和已撤销发送方的信令会被过滤。
func (store *InventoryStore) RTCOffers(ctx context.Context, device string, token [32]byte) ([]peer.RTCOffer, error) {
	offers := []peer.RTCOffer{}
	err := pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT s.transfer_id,s.attempt_id,s.offer FROM muster_rtc_signals s
			JOIN muster_transfers tr ON tr.id=s.transfer_id
			JOIN muster_devices d ON d.device_id=tr.source_device_id
			WHERE tr.target_device_id=$1 AND tr.received_at IS NULL AND d.revoked_at IS NULL
			AND s.expires_at>statement_timestamp() AND s.answer=''
			ORDER BY s.expires_at,s.transfer_id LIMIT $2`, device, peer.OfferLimit)
		if err != nil {
			return err
		}
		offers, err = pgx.AppendRows(offers, rows, func(row pgx.CollectableRow) (offer peer.RTCOffer, err error) {
			err = row.Scan(&offer.TransferID, &offer.AttemptID, &offer.SDP)
			return
		})
		return err
	})
	return offers, err
}

// PutRTCOffer 允许发送设备为仍有有效票据的待收投递注册 45 秒有效的连接提议。
// 同一尝试重发相同 SDP 时保留原应答和到期时间。新的尝试替换当前信令。
func (store *InventoryStore) PutRTCOffer(ctx context.Context, device string, token [32]byte, transfer string, signal peer.RTCSignal) error {
	return pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		// 在写入信令时核对投递票据、收件设备撤销状态和信令租期。
		result, err := tx.Exec(ctx, `INSERT INTO muster_rtc_signals(transfer_id,attempt_id,offer,expires_at)
			SELECT tr.id,$3,$4,statement_timestamp()+interval '45 seconds' FROM muster_transfers tr
			JOIN muster_devices d ON d.device_id=tr.target_device_id
			JOIN muster_rtc_devices rtc ON rtc.device_id=tr.target_device_id
			WHERE tr.id=$1 AND tr.source_device_id=$2 AND tr.received_at IS NULL
			AND tr.ticket_expires_at>statement_timestamp() AND d.revoked_at IS NULL AND rtc.expires_at>statement_timestamp()
			ON CONFLICT(transfer_id) DO UPDATE SET attempt_id=EXCLUDED.attempt_id,offer=EXCLUDED.offer,
				answer=CASE WHEN muster_rtc_signals.attempt_id=EXCLUDED.attempt_id THEN muster_rtc_signals.answer ELSE '' END,
				expires_at=CASE WHEN muster_rtc_signals.attempt_id=EXCLUDED.attempt_id THEN muster_rtc_signals.expires_at ELSE EXCLUDED.expires_at END
			WHERE muster_rtc_signals.attempt_id<>EXCLUDED.attempt_id OR
				(muster_rtc_signals.offer=EXCLUDED.offer AND muster_rtc_signals.expires_at>statement_timestamp())`,
			transfer, device, signal.AttemptID, signal.SDP)
		if err == nil && result.RowsAffected() == 0 {
			return workspace.ErrConflict
		}
		return err
	})
}

// RTCAnswer 仅向发送设备返回指定投递及连接尝试的有效应答。空 SDP 表示仍在等待。
func (store *InventoryStore) RTCAnswer(ctx context.Context, device string, token [32]byte, transfer, attempt string) (answer peer.RTCAnswer, err error) {
	err = pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		return notFound(tx.QueryRow(ctx, `SELECT s.answer FROM muster_rtc_signals s
			JOIN muster_transfers tr ON tr.id=s.transfer_id
			JOIN muster_devices d ON d.device_id=tr.target_device_id
			WHERE s.transfer_id=$1 AND s.attempt_id=$3 AND tr.source_device_id=$2
			AND d.revoked_at IS NULL AND s.expires_at>statement_timestamp()`, transfer, device, attempt).Scan(&answer.SDP))
	})
	return
}

// PutRTCAnswer 允许收件设备应答当前连接尝试。相同应答可重发，旧尝试和冲突应答返回冲突。
func (store *InventoryStore) PutRTCAnswer(ctx context.Context, device string, token [32]byte, transfer string, signal peer.RTCSignal) error {
	return pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE muster_rtc_signals s SET answer=$4
			FROM muster_transfers tr,muster_devices d
			WHERE s.transfer_id=$1 AND s.attempt_id=$3 AND tr.id=s.transfer_id AND tr.target_device_id=$2
			AND tr.source_device_id=d.device_id AND d.revoked_at IS NULL AND tr.received_at IS NULL
			AND s.expires_at>statement_timestamp() AND s.answer IN ('',$4)`, transfer, device, signal.AttemptID, signal.SDP)
		if err == nil && result.RowsAffected() == 0 {
			return workspace.ErrConflict
		}
		return err
	})
}

// RTCDiagnosticRole 验证设备属于指定投递。指定 attempt 时还要求当前信令未过期。
// 返回 sender 或 receiver。用空 attempt 查询登记期间的诊断资格。
func (store *InventoryStore) RTCDiagnosticRole(ctx context.Context, device string, token [32]byte, transfer, attempt string) (role string, err error) {
	err = pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		return notFound(tx.QueryRow(ctx, `
			SELECT CASE WHEN tr.source_device_id=$2 THEN 'sender' ELSE 'receiver' END
			FROM muster_transfers tr
			WHERE tr.id=$1 AND $2 IN (tr.source_device_id, tr.target_device_id)
			AND (($3='' AND tr.received_at IS NULL) OR EXISTS (
				SELECT 1 FROM muster_rtc_signals s WHERE s.transfer_id=tr.id
				AND s.attempt_id=$3 AND s.expires_at>statement_timestamp()
			))`, transfer, device, attempt).Scan(&role))
	})
	return
}

func hasRTCPeer(ctx context.Context, tx pgx.Tx, device string) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM muster_rtc_devices r JOIN muster_devices d USING(device_id)
		WHERE r.device_id=$1 AND d.revoked_at IS NULL AND r.expires_at>statement_timestamp())`, device).Scan(&active)
	return active, err
}
