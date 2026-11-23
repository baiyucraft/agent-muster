package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"reflect"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

func authenticateDevice(ctx context.Context, tx pgx.Tx, id string, token [32]byte) error {
	var authorized string
	err := tx.QueryRow(ctx, `SELECT device_id FROM muster_devices
		WHERE device_id=$1 AND token_hash=$2 AND revoked_at IS NULL FOR SHARE`,
		id, token[:]).Scan(&authorized)
	if errors.Is(err, pgx.ErrNoRows) {
		return registry.ErrUnauthorized
	}
	return err
}

// ReplaceDirectories 用认证设备提交的完整清单替换该设备的目录登记。
func (store *InventoryStore) ReplaceDirectories(ctx context.Context, id string, token [32]byte, directories []workspace.Directory) error {
	return store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, id, token); err != nil {
			return err
		}
		changed := false
		ids := make([]string, 0, len(directories))
		for _, directory := range directories {
			ids = append(ids, directory.ID)
			tag, err := tx.Exec(ctx, `
				INSERT INTO muster_directories (device_id,directory_id,label) VALUES ($1,$2,$3)
				ON CONFLICT (device_id,directory_id) DO UPDATE SET label=EXCLUDED.label
				WHERE muster_directories.label IS DISTINCT FROM EXCLUDED.label`, id, directory.ID, directory.Label)
			if err != nil {
				return err
			}
			changed = changed || tag.RowsAffected() != 0
		}
		removed, err := tx.Exec(ctx, "DELETE FROM muster_directories WHERE device_id=$1 AND NOT (directory_id=ANY($2::text[]))", id, ids)
		if err != nil {
			return err
		}
		if changed || removed.RowsAffected() != 0 {
			return advanceWorkspace(ctx, tx)
		}
		return nil
	})
}

// ClaimRun 为认证设备领取可执行运行，优先返回已领取且仍可执行的当前运行。
// 重领保留原运行身份及原生会话输入，并延长租约。
func (store *InventoryStore) ClaimRun(ctx context.Context, device string, token [32]byte) (claim workspace.RunClaim, err error) {
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		var firstClaim bool
		err := tx.QueryRow(ctx, `
			WITH choice AS (
				SELECT r.id,t.session_id,(r.claimed_at IS NULL) AS first_claim FROM muster_runs r
				JOIN muster_tasks t ON r.id=t.current_run_id AND r.task_id=t.id
				WHERE r.device_id=$1 AND r.status IN ('queued','running') AND t.status IN ('queued','running')
				ORDER BY (r.claimed_at IS NOT NULL) DESC,
					CASE WHEN r.created_at<=clock_timestamp()-interval '5 minutes' THEN 0 ELSE r.ready_priority END,
					r.sequence LIMIT 1
			)
			UPDATE muster_runs r SET claimed_at=COALESCE(r.claimed_at,clock_timestamp()),
				input_session=CASE WHEN choice.first_claim THEN choice.session_id ELSE r.input_session END,
				lease=COALESCE(NULLIF(r.lease,''),$2),lease_until=clock_timestamp()+interval '60 seconds'
			FROM choice,muster_tasks t LEFT JOIN muster_tasks parent ON parent.id=t.parent_id
			WHERE r.id=choice.id AND t.id=r.task_id
			RETURNING r.id,t.id,t.directory_id,r.input,r.input_session,t.agent_kind,t.project_id,r.generation,
				r.lease,r.lease_until,t.binding_id,COALESCE(t.parent_id,''),COALESCE(parent.device_id,''),t.root_id,
				r.prompt_transfer_id,r.dependency_results,choice.first_claim`, device, rand.Text()).
			Scan(&claim.ID, &claim.TaskID, &claim.DirectoryID, &claim.Prompt, &claim.SessionID, &claim.AgentKind,
				&claim.ProjectID, &claim.Generation, &claim.Lease, &claim.LeaseUntil, &claim.BindingID, &claim.ParentTaskID,
				&claim.ParentDeviceID, &claim.RootTaskID, &claim.PromptTransferID, &claim.DependencyResults, &firstClaim)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		claim.LeaseUntil = claim.LeaseUntil.UTC()
		claim.ApprovalSupported = true
		claim.Attachments, err = selectRows(ctx, tx, "SELECT "+transferColumns+` FROM muster_run_transfers rt
			JOIN muster_transfers tr ON tr.id=rt.transfer_id WHERE rt.run_id=$1 AND rt.direction='input'
			ORDER BY tr.kind,tr.id`, scanTransfer, claim.ID)
		if err != nil {
			return err
		}
		claim.DelegationTargets, err = selectRows(ctx, tx, `SELECT p.id,p.name,b.id,v.device_id,v.name,b.agent_kind,d.label
			FROM muster_project_bindings b JOIN muster_projects p ON b.project_id=p.id
			JOIN muster_devices v ON v.device_id=b.device_id
			JOIN muster_directories d ON d.device_id=b.device_id AND d.directory_id=b.directory_id
			WHERE v.device_id<>$1 AND v.revoked_at IS NULL
			ORDER BY (p.id=$2) DESC,p.name,b.is_default DESC,v.name,b.id LIMIT 64`,
			func(row pgx.Row) (workspace.DelegationTarget, error) {
				var target workspace.DelegationTarget
				err := row.Scan(&target.ProjectID, &target.ProjectName, &target.BindingID, &target.DeviceID,
					&target.DeviceName, &target.AgentKind, &target.DirectoryLabel)
				return target, err
			}, device, claim.ProjectID)
		if err == nil && firstClaim {
			return advanceWorkspace(ctx, tx)
		}
		return err
	})
	return
}

type runState struct {
	TaskID         string
	CurrentRun     string
	ParentTaskID   string
	ParentDeviceID string
	RootTaskID     string
	Status         string
	WaitingReason  string
	SessionID      string
	Generation     int64
	Lease          string
	LeaseLive      bool
	LegacyEvents   bool
}

// RenewRun 校验设备、当前运行、代次及有效租约后延长租期，返回新的到期时间。
func (store *InventoryStore) RenewRun(ctx context.Context, device string, token [32]byte, id string, generation int64, lease string) (time.Time, error) {
	var until time.Time
	err := pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE muster_runs r SET lease_until=clock_timestamp()+interval '60 seconds'
			FROM muster_tasks t WHERE r.id=$1 AND r.device_id=$2 AND r.generation=$3 AND r.lease=$4
			AND r.lease_until>=clock_timestamp() AND r.status IN ('queued','running') AND t.current_run_id=r.id
			RETURNING r.lease_until`, id, device, generation, lease).Scan(&until)
		if errors.Is(err, pgx.ErrNoRows) {
			return workspace.ErrConflict
		}
		return err
	})
	return until.UTC(), err
}

func readRunState(ctx context.Context, tx pgx.Tx, device, id string) (value runState, err error) {
	err = tx.QueryRow(ctx, `SELECT t.id,t.current_run_id,COALESCE(t.parent_id,''),COALESCE(p.device_id,''),t.root_id,r.status,t.waiting_reason,
		COALESCE(NULLIF(r.session_id,''),r.input_session),r.generation,r.lease,
		COALESCE(r.lease_until>=clock_timestamp(),false),r.legacy_events
		FROM muster_runs r JOIN muster_tasks t ON t.id=r.task_id LEFT JOIN muster_tasks p ON t.parent_id=p.id
		WHERE r.id=$1 AND r.device_id=$2 AND r.claimed_at IS NOT NULL`, id, device).
		Scan(&value.TaskID, &value.CurrentRun, &value.ParentTaskID, &value.ParentDeviceID, &value.RootTaskID,
			&value.Status, &value.WaitingReason, &value.SessionID, &value.Generation, &value.Lease, &value.LeaseLive, &value.LegacyEvents)
	return value, notFound(err)
}

// RecordRunEvent 按运行 ID 和递增序号记账，并对规范化后的相同事件去重。
// 已结束运行收到的有效事件只保留审计记录。
func (store *InventoryStore) RecordRunEvent(ctx context.Context, device string, token [32]byte, id string, sequence int64, event workspace.RunEvent) error {
	if event.Kind == "approval_requested" {
		if event.Approval == nil || len(event.WaitForTaskIDs)+len(event.TransferIDs) != 0 {
			return workspace.ErrInvalidApproval
		}
		question, err := event.Approval.Normalize()
		if err != nil {
			return workspace.ErrInvalidApproval
		}
		event.Approval = &question
	} else if event.Approval != nil {
		return workspace.ErrInvalidApproval
	}
	return store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		run, err := readRunState(ctx, tx, device, id)
		if err != nil {
			return err
		}
		legacy := run.LegacyEvents && event.Generation == 0 && event.Lease == ""
		if !legacy && (event.Lease == "" || event.Lease != run.Lease || event.Generation != run.Generation) {
			return workspace.ErrConflict
		}
		if run.ParentTaskID != "" {
			event.Text = ""
		}
		event.Lease = ""
		if len(event.WaitForTaskIDs) == 0 {
			event.WaitForTaskIDs = nil
		}
		if len(event.TransferIDs) == 0 {
			event.TransferIDs = nil
		}
		var previous workspace.RunEvent
		err = tx.QueryRow(ctx, "SELECT payload FROM muster_run_events WHERE run_id=$1 AND sequence=$2", id, sequence).Scan(&previous)
		if err == nil {
			if reflect.DeepEqual(previous, event) {
				return nil
			}
			return workspace.ErrConflict
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var last int64
		if err := tx.QueryRow(ctx, "SELECT COALESCE(MAX(sequence),0) FROM muster_run_events WHERE run_id=$1", id).Scan(&last); err != nil {
			return err
		}
		if sequence <= last {
			return workspace.ErrConflict
		}
		sessionID := run.SessionID
		if event.SessionID != "" {
			if sessionID != "" && sessionID != event.SessionID {
				return workspace.ErrConflict
			}
			sessionID = event.SessionID
		}
		if _, err := tx.Exec(ctx, "INSERT INTO muster_run_events(run_id,sequence,payload) VALUES($1,$2,$3)", id, sequence, event); err != nil {
			return err
		}
		if run.Status != "queued" && run.Status != "running" {
			// Preserve evidence for a settled round without changing task state or scheduling.
			return advanceWorkspace(ctx, tx)
		}
		switch event.Kind {
		case "approval_requested":
			if legacy {
				return workspace.ErrConflict
			}
			if err := store.recordApproval(ctx, tx, run, id, sessionID, *event.Approval); err != nil {
				return err
			}
		case "waiting":
			if sessionID == "" || len(event.WaitForTaskIDs)+len(event.TransferIDs) == 0 || legacy {
				return workspace.ErrConflict
			}
			if err := recordWait(ctx, tx, run, id, device, event.WaitForTaskIDs, event.TransferIDs); err != nil {
				return err
			}
		case "completed", "failed", "interrupted":
			if event.Kind == "completed" && run.ParentTaskID != "" && len(event.TransferIDs) == 0 {
				return workspace.ErrConflict
			}
			if err := recordResultTransfers(ctx, tx, run, id, event.TransferIDs); err != nil {
				return err
			}
		}
		status := event.Kind
		terminal := status != "started" && status != "session"
		if !terminal {
			status = "running"
		}
		if _, err := tx.Exec(ctx, `UPDATE muster_runs SET status=$2,session_id=$3,outcome=$4,updated_at=clock_timestamp()
			WHERE id=$1`, id, status, sessionID, event.Text); err != nil {
			return err
		}
		switch event.Kind {
		case "approval_requested", "waiting", "completed":
			if err := finishApproval(ctx, tx, id); err != nil {
				return err
			}
		}
		if run.CurrentRun == id && event.Kind != "approval_requested" {
			if err := updateTaskFromRun(ctx, tx, run, id, status, sessionID, event.Text, terminal, len(event.WaitForTaskIDs) > 0); err != nil {
				return err
			}
		}
		return advanceWorkspace(ctx, tx)
	})
}

func updateTaskFromRun(ctx context.Context, tx pgx.Tx, run runState, runID, status, sessionID, outcome string, ended, waitingChildren bool) error {
	reason := run.WaitingReason
	if status == "waiting" {
		reason = "transfers"
		if waitingChildren {
			reason = "children"
		}
	}
	if ended && sessionID != "" && status != "interrupted" {
		var next string
		err := tx.QueryRow(ctx, `SELECT id FROM muster_runs WHERE task_id=$1 AND id<>$2 AND status='queued' ORDER BY sequence LIMIT 1`,
			run.TaskID, runID).Scan(&next)
		if err == nil {
			_, err := tx.Exec(ctx, `UPDATE muster_tasks SET status='queued',waiting_reason=$4,current_run_id=$2,
				session_id=$3,updated_at=clock_timestamp() WHERE id=$1`, run.TaskID, next, sessionID, reason)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	taskStatus := status
	if ended && sessionID != "" && status != "interrupted" && reason != "" {
		taskStatus = "waiting"
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_tasks SET status=$2,session_id=$3,result=$4,waiting_reason=$5,
		updated_at=clock_timestamp() WHERE id=$1`, run.TaskID, taskStatus, sessionID, outcome, reason); err != nil {
		return err
	}
	if !ended {
		return nil
	}
	if taskStatus == "waiting" {
		return reconcileWait(ctx, tx, run.TaskID)
	}
	if reason != "" {
		return nil
	}
	if sessionID == "" || status == "interrupted" {
		if _, err := tx.Exec(ctx, `
			UPDATE muster_runs SET status='interrupted',outcome=CASE WHEN $3 THEN '' ELSE '上一轮的结果待核对，本轮尚未启动' END,
				updated_at=clock_timestamp()
			WHERE task_id=$1 AND id<>$2 AND status='queued'`, run.TaskID, runID, run.ParentTaskID != ""); err != nil {
			return err
		}
	}
	if run.ParentTaskID != "" {
		return reconcileParent(ctx, tx, run.TaskID)
	}
	_, err := tx.Exec(ctx, `INSERT INTO muster_deliveries(id,kind,body,task_id)
		SELECT $1,'report',title || E'\n运行状态: ' || $2 || E'\n' || $3,id FROM muster_tasks
		WHERE id=$4 ON CONFLICT (id) DO NOTHING`, "report-"+runID, status, outcome, run.TaskID)
	return err
}

const (
	priorityFollowUp   = 1
	priorityDependency = 2
	priorityNew        = 3
)

func nextRun(ctx context.Context, tx pgx.Tx, taskID, runID, prompt, promptTransferID string, priority int) error {
	_, err := tx.Exec(ctx, `INSERT INTO muster_runs(id,task_id,input,device_id,generation,prompt_transfer_id,ready_priority)
		SELECT $2,t.id,$3,t.device_id,COALESCE((SELECT MAX(r.generation) FROM muster_runs r WHERE r.task_id=t.id),0)+1,$4,$5
		FROM muster_tasks t WHERE t.id=$1`, taskID, runID, prompt, promptTransferID, priority)
	return err
}
