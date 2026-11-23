package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

//go:embed migrations/020_session_takeovers.sql
var addSessionTakeovers string

// 已确认退出的原轮次和可用目录是重新接手的前置条件。未核对的推送意图需先完成远端回读。
const eligibleTakeover = `t.status='interrupted' AND t.waiting_reason='session_unavailable'
	AND t.control_mode='' AND NOT t.center_group
	AND EXISTS (SELECT 1 FROM muster_runs r WHERE r.id=t.current_run_id AND r.task_id=t.id AND r.status='interrupted')
	AND EXISTS (SELECT 1 FROM muster_directories d JOIN muster_devices v USING(device_id)
		WHERE d.device_id=t.device_id AND d.directory_id=t.directory_id AND v.revoked_at IS NULL)
	AND NOT EXISTS (SELECT 1 FROM muster_runs r WHERE r.task_id=t.id AND r.claimed_at IS NOT NULL AND r.status IN ('queued','running'))
	AND NOT EXISTS (SELECT 1 FROM muster_git_push_intents p WHERE p.task_id=t.id AND p.status IN ('pending','unavailable'))
	AND NOT EXISTS (SELECT 1 FROM muster_session_takeovers x WHERE x.source_task_id=t.id)`

// TakeOverTask 使用管理员提供的摘要，从不可用会话的任务创建一个关联的新根任务。
// 相同请求及摘要返回已保存的任务。新任务重新启动原绑定的 Agent，并沿用原 Git 验证规则。
func (store *InventoryStore) TakeOverTask(ctx context.Context, source string, input workspace.TaskTakeover) (task workspace.Task, err error) {
	digest := sha256.Sum256([]byte(input.Summary))
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		var replayID, requestID string
		var storedDigest []byte
		err := tx.QueryRow(ctx, `SELECT task_id,request_id::text,summary_digest FROM muster_session_takeovers WHERE source_task_id=$1`, source).
			Scan(&replayID, &requestID, &storedDigest)
		if err == nil {
			if requestID != input.RequestID || !bytes.Equal(storedDigest, digest[:]) {
				return workspace.ErrConflict
			}
			task, err = scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM muster_tasks t WHERE t.id=$1", replayID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var route taskRoute
		var title string
		var allowed bool
		err = tx.QueryRow(ctx, `SELECT COALESCE(t.project_id,''),t.project_name,COALESCE(t.binding_id,''),
			COALESCE(t.device_id,''),COALESCE(t.directory_id,''),t.agent_kind,t.git_policy,t.title,(`+eligibleTakeover+`)
			FROM muster_tasks t WHERE t.id=$1`, source).
			Scan(&route.ProjectID, &route.ProjectName, &route.BindingID, &route.DeviceID, &route.DirectoryID, &route.AgentKind, &route.GitPolicy, &title, &allowed)
		if err != nil {
			return notFound(err)
		}
		if !allowed {
			return workspace.ErrConflict
		}
		// 目录登记受工作区写入锁保护。锁住设备记录，确保并发撤销不会与任务创建交错。
		var device string
		if err := tx.QueryRow(ctx, `SELECT device_id FROM muster_devices WHERE device_id=$1 AND revoked_at IS NULL FOR SHARE`, route.DeviceID).Scan(&device); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return workspace.ErrConflict
			}
			return err
		}
		id := "task-" + stableUUID(source+"\x00takeover\x00"+input.RequestID)
		runID := "run-" + stableUUID(id+"\x00first-run")
		if err := insertTask(ctx, tx, id, runID, title, "", id, route); err != nil {
			return err
		}
		if err := nextRun(ctx, tx, id, runID, input.Summary, "", priorityNew); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO muster_session_takeovers(source_task_id,request_id,summary_digest,task_id)
			VALUES($1,$2::text::uuid,$3,$4)`, source, input.RequestID, digest[:], id); err != nil {
			return err
		}
		task, err = scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM muster_tasks t WHERE t.id=$1", id))
		if err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
	return task, err
}
