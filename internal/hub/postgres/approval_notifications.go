package postgres

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/baiyucraft/agent-muster/internal/hub/im"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

var _ im.Store = (*InventoryStore)(nil)

// EnableApprovalNotifications 绑定通知目标，并为尚未绑定渠道的待答复请示登记通知。
// 只在 Hub 开始处理请求前调用。旧通知保留创建时绑定的应用和用户。
func (store *InventoryStore) EnableApprovalNotifications(ctx context.Context, recipient im.Recipient) error {
	if recipient.AppID == "" || recipient.OpenID == "" {
		return errors.New("请示通知需要完整的应用和接收账号")
	}
	err := store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO muster_approval_notifications(approval_id,app_id,open_id,desired_status)
			SELECT id,$1,$2,'pending' FROM muster_approvals WHERE answered_at IS NULL
			ON CONFLICT (approval_id) DO NOTHING`, recipient.AppID, recipient.OpenID)
		return err
	})
	if err == nil {
		store.recipient = recipient
	}
	return err
}

// ClaimApprovalNotification 领取一条到期的通知，返回 30 秒租约及当时应投递的卡片状态。
// 创建卡片的重试保持原始待答复状态，留出 5 分钟余量防止超过渠道为期一小时的发送去重窗口。
// 飞书只允许更新 14 天内发出的卡片。以首次发送尝试为上限起点，并为请求留出一个租约的时间。
func (store *InventoryStore) ClaimApprovalNotification(ctx context.Context, recipient im.Recipient) (delivery im.Delivery, err error) {
	if recipient.AppID == "" || recipient != store.recipient {
		return delivery, nil
	}
	err = pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		var approvalID, state string
		lease := rand.Text()
		err := tx.QueryRow(ctx, `WITH job AS (
			SELECT approval_id FROM muster_approval_notifications
			WHERE app_id=$1 AND open_id=$2 AND sent_status IS DISTINCT FROM desired_status
			AND next_attempt_at<=clock_timestamp() AND (lease_until IS NULL OR lease_until<=clock_timestamp())
			AND ((message_id IS NOT NULL AND first_send_at>clock_timestamp()-interval '336 hours'+interval '30 seconds')
				OR (message_id IS NULL AND (
					(first_send_at IS NULL AND desired_status='pending')
					OR first_send_at>clock_timestamp()-interval '55 minutes'
				)))
			ORDER BY next_attempt_at,approval_id LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE muster_approval_notifications n SET lease=$3,lease_until=clock_timestamp()+interval '30 seconds',
			leased_status=CASE WHEN message_id IS NULL THEN 'pending' ELSE desired_status END,
			first_send_at=COALESCE(first_send_at,clock_timestamp())
		FROM job WHERE n.approval_id=job.approval_id
		RETURNING n.approval_id,COALESCE(n.message_id,''),n.leased_status`, recipient.AppID, recipient.OpenID, lease).
			Scan(&approvalID, &delivery.MessageID, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		delivery.Approval, err = scanApproval(tx.QueryRow(ctx, "SELECT "+approvalColumns+" WHERE a.id=$1", approvalID))
		if err != nil {
			return err
		}
		delivery.Lease, delivery.Approval.Status = lease, state
		if state == "pending" {
			delivery.Approval.Answer, delivery.Approval.AnsweredAt = "", nil
		}
		if state != "handled" {
			delivery.Approval.HandledAt = nil
		}
		return nil
	})
	return delivery, err
}

// CompleteApprovalNotification 记录本次租约实际发送的状态及消息 ID。
// 此时若请示已有更新，下一轮继续投递新状态。
func (store *InventoryStore) CompleteApprovalNotification(ctx context.Context, recipient im.Recipient, delivery im.Delivery, messageID string) error {
	if messageID == "" || recipient.AppID == "" || recipient != store.recipient {
		return workspace.ErrConflict
	}
	tag, err := store.pool.Exec(ctx, `UPDATE muster_approval_notifications
		SET message_id=$7,sent_status=leased_status,attempts=0,next_attempt_at=clock_timestamp(),
			lease=NULL,lease_until=NULL,leased_status=NULL
		WHERE approval_id=$1 AND app_id=$2 AND open_id=$3 AND lease=$4 AND lease_until>clock_timestamp()
		AND leased_status=$5 AND COALESCE(message_id,'')=$6 AND (message_id IS NULL OR message_id=$7)`,
		delivery.Approval.ID, recipient.AppID, recipient.OpenID, delivery.Lease, delivery.Approval.Status, delivery.MessageID, messageID)
	if err == nil && tag.RowsAffected() == 0 {
		return workspace.ErrConflict
	}
	return err
}

// RetryApprovalNotification 释放现有租约，并登记一秒到一分钟的指数退避。
func (store *InventoryStore) RetryApprovalNotification(ctx context.Context, recipient im.Recipient, delivery im.Delivery) error {
	if recipient.AppID == "" || recipient != store.recipient {
		return workspace.ErrConflict
	}
	tag, err := store.pool.Exec(ctx, `UPDATE muster_approval_notifications
		SET next_attempt_at=clock_timestamp()+LEAST(60,1 << LEAST(attempts,6))*interval '1 second',
			attempts=LEAST(attempts+1,6),lease=NULL,lease_until=NULL,leased_status=NULL
		WHERE approval_id=$1 AND app_id=$2 AND open_id=$3 AND lease=$4 AND lease_until>clock_timestamp()
		AND leased_status=$5 AND COALESCE(message_id,'')=$6`,
		delivery.Approval.ID, recipient.AppID, recipient.OpenID, delivery.Lease, delivery.Approval.Status, delivery.MessageID)
	if err == nil && tag.RowsAffected() == 0 {
		return workspace.ErrConflict
	}
	return err
}

// AnswerApprovalFromIM 核对私聊消息与应用、接收账号和请示的绑定，然后按首个有效答复安排续接。
// 卡片动作使用原始选项的零基序号，消息尚未确认时可在渠道重试。
func (store *InventoryStore) AnswerApprovalFromIM(ctx context.Context, recipient im.Recipient, reply im.Reply) (value workspace.Approval, err error) {
	if recipient.AppID == "" || recipient != store.recipient || reply.OpenID != recipient.OpenID || reply.MessageID == "" {
		return value, im.ErrReplyUnavailable
	}
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		value, err = scanApproval(tx.QueryRow(ctx, "SELECT "+approvalColumns+`
			JOIN muster_approval_notifications n ON n.approval_id=a.id
			WHERE n.app_id=$1 AND n.open_id=$2 AND n.message_id=$3 AND ($4='' OR a.id=$4)`,
			recipient.AppID, recipient.OpenID, reply.MessageID, reply.ApprovalID))
		if errors.Is(err, pgx.ErrNoRows) {
			return im.ErrReplyUnavailable
		}
		if err != nil {
			return err
		}
		answer := reply.Text
		if reply.Option != nil {
			if reply.Text != "" || *reply.Option < 0 || *reply.Option >= len(value.Options) {
				return workspace.ErrInvalidApproval
			}
			answer = value.Options[*reply.Option]
		}
		answer, err = normalizeApprovalAnswer(answer)
		if err != nil {
			return err
		}
		return saveApprovalAnswer(ctx, tx, &value, answer)
	})
	if errors.Is(err, workspace.ErrConflict) || errors.Is(err, registry.ErrNotFound) || errors.Is(err, workspace.ErrInvalidApproval) {
		err = im.ErrReplyUnavailable
	}
	return value, err
}
