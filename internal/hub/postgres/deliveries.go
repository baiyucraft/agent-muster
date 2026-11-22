package postgres

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

// ClaimDelivery 为中心插件串行领取一条待处理消息或根任务汇报，租期为 60 秒。
func (store *InventoryStore) ClaimDelivery(ctx context.Context) (delivery workspace.Delivery, err error) {
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			WITH next AS (
				SELECT d.id FROM muster_deliveries d
				WHERE NOT done AND available_at<=clock_timestamp()
				AND NOT (d.kind='report' AND EXISTS (
					SELECT 1 FROM muster_messages m WHERE m.requested_task_id=d.task_id AND m.status='accepted'
				))
				AND NOT EXISTS (
					SELECT 1 FROM muster_deliveries WHERE NOT done AND lease_until>clock_timestamp()
				)
				ORDER BY (lease_until IS NOT NULL) DESC,sequence LIMIT 1
			)
			UPDATE muster_deliveries d SET lease=$1,lease_until=clock_timestamp()+interval '60 seconds'
			FROM next WHERE d.id=next.id
			RETURNING d.id,d.lease,d.kind,d.body,COALESCE(d.message_id,''),COALESCE(d.task_id,''),
				COALESCE((SELECT project_id FROM muster_tasks WHERE id=d.task_id),''),d.attempt`, rand.Text()).
			Scan(&delivery.ID, &delivery.Lease, &delivery.Kind, &delivery.Text, &delivery.MessageID,
				&delivery.TaskID, &delivery.ProjectID, &delivery.Attempt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return delivery, err
}

// RenewDelivery 验证有效租约后，为同一笔中心插件投递延长 60 秒。
func (store *InventoryStore) RenewDelivery(ctx context.Context, id, lease string) error {
	return store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE muster_deliveries SET lease_until=clock_timestamp()+interval '60 seconds'
			WHERE id=$1 AND lease=$2 AND NOT done AND lease_until>=clock_timestamp()`, id, lease)
		if err == nil && tag.RowsAffected() == 0 {
			return workspace.ErrConflict
		}
		return err
	})
}

// CompleteDelivery 幂等处理插件回执，按处理结果更新主对话或安排延迟重试。
func (store *InventoryStore) CompleteDelivery(ctx context.Context, id string, input workspace.Completion) error {
	return store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		var kind, messageID, taskID, dispatched string
		var done, leased bool
		var previous *workspace.Completion
		err := tx.QueryRow(ctx, `
			SELECT kind,COALESCE(message_id,''),COALESCE(task_id,''),COALESCE(dispatch_task_id,''),
				done,COALESCE(lease=$2 AND lease_until>=clock_timestamp(),false),completion
			FROM muster_deliveries WHERE id=$1`, id, input.Lease).
			Scan(&kind, &messageID, &taskID, &dispatched, &done, &leased, &previous)
		if err != nil {
			return notFound(err)
		}
		if previous != nil && *previous == input {
			return nil
		}
		if done || !leased {
			return workspace.ErrConflict
		}
		if input.Status == "interrupted" || (kind == "report" && input.Text == "") {
			_, err := tx.Exec(ctx, `
				UPDATE muster_deliveries SET lease='',lease_until=NULL,available_at=clock_timestamp()+interval '20 seconds',
					attempt=attempt+1,completion=$2 WHERE id=$1`, id, input)
			return err
		}
		if kind == "message" {
			status := input.Status
			if dispatched != "" {
				status = "handled"
			}
			if _, err := tx.Exec(ctx, "UPDATE muster_messages SET status=$2 WHERE id=$1", messageID, status); err != nil {
				return err
			}
		}
		if input.Text != "" && dispatched == "" && input.Status == "handled" {
			messageKind := "reply"
			if kind == "report" {
				messageKind = "report"
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO muster_messages (id,kind,body,task_id)
				VALUES ($1,$2,$3,NULLIF($4,''))`, "reply-"+id, messageKind, input.Text, taskID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE muster_deliveries SET done=true,completion=$2 WHERE id=$1", id, input); err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
}

// Dispatch 用持有有效租约的消息创建根任务或追加运行。
// 同一条消息重新派发时返回首次持久化的任务 ID。
func (store *InventoryStore) Dispatch(ctx context.Context, id string, input workspace.Dispatch) (taskID string, err error) {
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		var kind, messageID, selectedTask string
		var done, leased bool
		err := tx.QueryRow(ctx, `
			SELECT kind,COALESCE(message_id,''),COALESCE(task_id,''),COALESCE(dispatch_task_id,''),done,
				COALESCE(lease=$2 AND lease_until>=clock_timestamp(),false)
			FROM muster_deliveries WHERE id=$1`, id, input.Lease).
			Scan(&kind, &messageID, &selectedTask, &taskID, &done, &leased)
		if err != nil {
			return notFound(err)
		}
		if kind != "message" || !leased || done {
			return workspace.ErrConflict
		}
		if taskID != "" {
			// The native turn can be replayed with a newly generated tool call ID.
			// Its first persisted dispatch owns the source message.
			return nil
		}
		if selectedTask != "" && input.TaskID != selectedTask {
			return workspace.ErrConflict
		}
		runID := "run-" + messageID
		if input.TaskID == "" {
			taskID = "task-" + messageID
			if err := createTask(ctx, tx, taskID, runID, input); err != nil {
				return err
			}
		} else {
			taskID = input.TaskID
			if err := appendRun(ctx, tx, taskID, runID); err != nil {
				return err
			}
		}
		priority := priorityNew
		if input.TaskID != "" {
			priority = priorityFollowUp
		}
		if err := nextRun(ctx, tx, taskID, runID, input.Prompt, "", priority); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE muster_messages SET task_id=$2,status='handled' WHERE id=$1", messageID, taskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE muster_deliveries SET dispatch_task_id=$2 WHERE id=$1", id, taskID); err != nil {
			return err
		}
		if input.TaskID != "" {
			if _, err := tx.Exec(ctx, `UPDATE muster_deliveries SET done=true
				WHERE task_id=$1 AND kind='report' AND NOT done`, taskID); err != nil {
				return err
			}
		}
		return advanceWorkspace(ctx, tx)
	})
	return taskID, err
}

func createTask(ctx context.Context, tx pgx.Tx, taskID, runID string, input workspace.Dispatch) error {
	route, err := resolveBinding(ctx, tx, input.ProjectID, input.BindingID)
	if err != nil {
		return err
	}
	return insertTask(ctx, tx, taskID, runID, input.Title, "", taskID, route)
}

type taskRoute struct {
	ProjectID   string
	ProjectName string
	BindingID   string
	DeviceID    string
	DirectoryID string
	AgentKind   string
}

func resolveBinding(ctx context.Context, tx pgx.Tx, projectID, bindingID string) (route taskRoute, err error) {
	err = tx.QueryRow(ctx, `SELECT p.id,p.name,b.id,b.device_id,b.directory_id,b.agent_kind
		FROM muster_projects p JOIN muster_project_bindings b ON b.project_id=p.id
		JOIN muster_directories d ON d.device_id=b.device_id AND d.directory_id=b.directory_id
		JOIN muster_devices v ON v.device_id=b.device_id
		WHERE p.id=$1 AND (($2='' AND b.is_default) OR b.id=$2) AND v.revoked_at IS NULL FOR SHARE OF v`,
		projectID, bindingID).Scan(&route.ProjectID, &route.ProjectName, &route.BindingID, &route.DeviceID, &route.DirectoryID, &route.AgentKind)
	if errors.Is(err, pgx.ErrNoRows) {
		err = workspace.ErrConflict
	}
	return
}

func insertTask(ctx context.Context, tx pgx.Tx, taskID, runID, title, parentID, rootID string, route taskRoute) error {
	status, reason := "queued", ""
	if parentID != "" {
		status, reason = "waiting", "delivery"
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO muster_tasks (id,project_id,project_name,device_id,directory_id,title,status,current_run_id,
			parent_id,root_id,binding_id,agent_kind,waiting_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12,$13) ON CONFLICT (id) DO NOTHING`,
		taskID, route.ProjectID, route.ProjectName, route.DeviceID, route.DirectoryID, title, status, runID, parentID, rootID,
		route.BindingID, route.AgentKind, reason)
	if err == nil && tag.RowsAffected() == 0 {
		return workspace.ErrConflict
	}
	return err
}

func appendRun(ctx context.Context, tx pgx.Tx, taskID, runID string) error {
	var status, session, parent string
	err := tx.QueryRow(ctx, `
		SELECT t.status,t.session_id,COALESCE(t.parent_id,'') FROM muster_tasks t
		JOIN muster_directories d USING (device_id,directory_id)
		JOIN muster_devices v USING (device_id) WHERE t.id=$1 AND v.revoked_at IS NULL`, taskID).
		Scan(&status, &session, &parent)
	if err != nil {
		return notFound(err)
	}
	if parent != "" {
		return registry.ErrNotFound
	}
	if status == "running" || status == "queued" {
		return nil
	}
	if session == "" {
		return workspace.ErrConflict
	}
	_, err = tx.Exec(ctx, `
		UPDATE muster_tasks SET status='queued',current_run_id=$2,updated_at=clock_timestamp() WHERE id=$1`, taskID, runID)
	return err
}
