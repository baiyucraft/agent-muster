package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

const transferColumns = `tr.id,tr.run_id,tr.task_id,tr.source_device_id,tr.target_device_id,tr.name,tr.kind,
	tr.sha256,tr.size,tr.received_at,tr.attempts,tr.created_at,tr.last_error,
	CASE WHEN tr.received_at IS NOT NULL THEN 'received' WHEN tr.attempts>1 OR tr.last_error<>'' THEN 'retrying' ELSE 'pending' END`

func scanTransfer(row pgx.Row) (value workspace.Transfer, err error) {
	err = row.Scan(&value.ID, &value.RunID, &value.TaskID, &value.SourceDeviceID, &value.TargetDeviceID, &value.Name,
		&value.Kind, &value.SHA256, &value.Size, &value.ReceivedAt, &value.Attempts, &value.CreatedAt, &value.LastError, &value.Status)
	return
}

// RegisterPeer 为认证设备登记直连接收地址和证书指纹。
func (store *InventoryStore) RegisterPeer(ctx context.Context, device string, token [32]byte, peer workspace.Peer) error {
	return store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO muster_device_peers(device_id,address,fingerprint) VALUES($1,$2,$3)
			ON CONFLICT(device_id) DO UPDATE SET address=EXCLUDED.address,fingerprint=EXCLUDED.fingerprint
			WHERE (muster_device_peers.address,muster_device_peers.fingerprint) IS DISTINCT FROM (EXCLUDED.address,EXCLUDED.fingerprint)`,
			device, peer.Address, strings.ToLower(peer.Fingerprint))
		return err
	})
}

// CreateTransfer 根据运行凭据和负载元数据创建投递并签发短期票据。
// 重试必须保留同一投递 ID 与不可变的投递元数据。
func (store *InventoryStore) CreateTransfer(ctx context.Context, device string, token [32]byte, runID string, input workspace.TransferRequest) (transfer workspace.Transfer, err error) {
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		run, err := readRunState(ctx, tx, device, runID)
		if err != nil {
			return err
		}
		if input.Lease == "" || run.Lease != input.Lease || run.Generation != input.Generation {
			return workspace.ErrConflict
		}
		input.SHA256 = strings.ToLower(input.SHA256)
		decoded, err := hex.DecodeString(input.SHA256)
		if err != nil || len(decoded) != sha256.Size || input.Size < 0 || input.Size > 32*1024*1024 {
			return workspace.ErrConflict
		}
		switch input.Kind {
		case "prompt", "file", "result":
		case "message":
			if input.Size > 64*1024 {
				return workspace.ErrConflict
			}
		default:
			return workspace.ErrConflict
		}
		if input.Kind == "result" && run.ParentDeviceID != input.TargetDeviceID {
			return workspace.ErrConflict
		}
		if input.TargetDeviceID == device {
			return workspace.ErrConflict
		}
		transfer, err = scanTransfer(tx.QueryRow(ctx, "SELECT "+transferColumns+" FROM muster_transfers tr WHERE tr.id=$1", input.ID))
		if err == nil {
			if transfer.RunID != runID || transfer.SourceDeviceID != device || transfer.TargetDeviceID != input.TargetDeviceID ||
				transfer.Name != input.Name || transfer.Kind != input.Kind || transfer.SHA256 != input.SHA256 || transfer.Size != input.Size {
				return workspace.ErrConflict
			}
			if transfer.ReceivedAt != nil {
				return nil
			}
			if err := mintTransferTicket(ctx, tx, &transfer); err != nil {
				return err
			}
			transfer.Status = "retrying"
			return advanceWorkspace(ctx, tx)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if run.CurrentRun != runID || run.Status != "running" || (!run.LeaseLive && input.Kind != "result") {
			return workspace.ErrConflict
		}
		var target string
		if err := tx.QueryRow(ctx, "SELECT device_id FROM muster_devices WHERE device_id=$1 AND revoked_at IS NULL FOR SHARE", input.TargetDeviceID).Scan(&target); err != nil {
			return notFound(err)
		}
		transfer, err = scanTransfer(tx.QueryRow(ctx, `INSERT INTO muster_transfers AS tr
			(id,run_id,task_id,source_device_id,target_device_id,name,kind,sha256,size)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+transferColumns,
			input.ID, runID, run.TaskID, device, target, input.Name, input.Kind, input.SHA256, input.Size))
		if err != nil {
			return err
		}
		if err := mintTransferTicket(ctx, tx, &transfer); err != nil {
			return err
		}
		transfer.Peer, err = transferPeer(ctx, tx, transfer.TargetDeviceID)
		if err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
	return
}

func mintTransferTicket(ctx context.Context, tx pgx.Tx, transfer *workspace.Transfer) error {
	var ticket [32]byte
	_, err := rand.Read(ticket[:])
	if err != nil {
		return err
	}
	transfer.Ticket = hex.EncodeToString(ticket[:])
	hash := sha256.Sum256([]byte(transfer.Ticket))
	return tx.QueryRow(ctx, `UPDATE muster_transfers SET ticket_hash=$2,
		ticket_expires_at=clock_timestamp()+interval '10 minutes',attempts=attempts+1 WHERE id=$1
		RETURNING ticket_expires_at,attempts`, transfer.ID, hash[:]).Scan(&transfer.ExpiresAt, &transfer.Attempts)
}

func transferPeer(ctx context.Context, tx pgx.Tx, id string) (*workspace.Peer, error) {
	var peer workspace.Peer
	err := tx.QueryRow(ctx, `SELECT p.device_id,p.address,p.fingerprint FROM muster_device_peers p
		JOIN muster_devices d USING(device_id) WHERE p.device_id=$1 AND d.revoked_at IS NULL`, id).
		Scan(&peer.DeviceID, &peer.Address, &peer.Fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &peer, err
}

// OutgoingTransfers 列出认证设备发往有效设备且尚未签收的投递。
func (store *InventoryStore) OutgoingTransfers(ctx context.Context, device string, token [32]byte) (transfers []workspace.Transfer, err error) {
	err = pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		var err error
		transfers, err = selectRows(ctx, tx, "SELECT "+transferColumns+` FROM muster_transfers tr
			JOIN muster_devices d ON d.device_id=tr.target_device_id
			WHERE tr.source_device_id=$1 AND tr.received_at IS NULL AND d.revoked_at IS NULL ORDER BY tr.created_at,tr.id`,
			scanTransfer, device)
		if err != nil {
			return err
		}
		for i := range transfers {
			transfers[i].Peer, err = transferPeer(ctx, tx, transfers[i].TargetDeviceID)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return
}

// AuthorizeTransfer 为认证发送设备刷新未签收投递的票据，目标须已登记可用接收端点。
func (store *InventoryStore) AuthorizeTransfer(ctx context.Context, device string, token [32]byte, id string) (transfer workspace.Transfer, err error) {
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		transfer, err = scanTransfer(tx.QueryRow(ctx, "SELECT "+transferColumns+
			" FROM muster_transfers tr WHERE tr.id=$1 AND tr.source_device_id=$2", id, device))
		if err != nil {
			return notFound(err)
		}
		if transfer.ReceivedAt != nil {
			return nil
		}
		transfer.Peer, err = transferPeer(ctx, tx, transfer.TargetDeviceID)
		if err != nil {
			return err
		}
		if transfer.Peer == nil {
			active, err := hasRTCPeer(ctx, tx, transfer.TargetDeviceID)
			if err != nil {
				return err
			}
			if !active {
				return workspace.ErrConflict
			}
		}
		if err := mintTransferTicket(ctx, tx, &transfer); err != nil {
			return err
		}
		transfer.Status = "retrying"
		return advanceWorkspace(ctx, tx)
	})
	return
}

// VerifyTransfer 校验接收设备和票据，返回对应的投递元数据。
func (store *InventoryStore) VerifyTransfer(ctx context.Context, device string, token [32]byte, id, ticket string) (workspace.Transfer, error) {
	var value workspace.Transfer
	err := pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		var err error
		value, err = receiverTransfer(ctx, tx, device, token, id, ticket)
		return err
	})
	return value, err
}

func receiverTransfer(ctx context.Context, tx pgx.Tx, device string, token [32]byte, id, ticket string) (workspace.Transfer, error) {
	if err := authenticateDevice(ctx, tx, device, token); err != nil {
		return workspace.Transfer{}, err
	}
	hash := sha256.Sum256([]byte(ticket))
	transfer, err := scanTransfer(tx.QueryRow(ctx, "SELECT "+transferColumns+` FROM muster_transfers tr
		JOIN muster_devices d ON d.device_id=tr.source_device_id WHERE tr.id=$1 AND tr.target_device_id=$2
		AND tr.ticket_hash=$3 AND (tr.ticket_expires_at>clock_timestamp() OR tr.received_at IS NOT NULL)
		AND d.revoked_at IS NULL`, id, device, hash[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return transfer, workspace.ErrConflict
	}
	return transfer, err
}

// AcknowledgeTransfer 幂等登记接收设备的签收，并推进依赖此投递的等待条件。
func (store *InventoryStore) AcknowledgeTransfer(ctx context.Context, device string, token [32]byte, id, ticket string) error {
	return store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		transfer, err := receiverTransfer(ctx, tx, device, token, id, ticket)
		if err != nil || transfer.ReceivedAt != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE muster_transfers SET received_at=clock_timestamp(),last_error='' WHERE id=$1", id); err != nil {
			return err
		}
		if err := reconcileInputTransfer(ctx, tx, id); err != nil {
			return err
		}
		if err := reconcileParent(ctx, tx, transfer.TaskID); err != nil {
			return err
		}
		if err := reconcileTransferWaiters(ctx, tx, id); err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
}

// ReportTransferError 为发送设备尚未签收的投递记录允许的错误原因。
func (store *InventoryStore) ReportTransferError(ctx context.Context, device string, token [32]byte, id, reason string) error {
	switch reason {
	case "peer_unavailable", "verification_failed", "delivery_failed":
	default:
		return workspace.ErrConflict
	}
	return store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE muster_transfers SET last_error=$3 WHERE id=$1 AND source_device_id=$2
			AND received_at IS NULL AND last_error<>$3`, id, device, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return advanceWorkspace(ctx, tx)
		}
		var owned bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM muster_transfers WHERE id=$1 AND source_device_id=$2)", id, device).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return notFound(pgx.ErrNoRows)
		}
		return nil
	})
}
