package postgres

import (
	"context"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

// validSessionFailure 校验原生会话采纳前的失败事件。诊断可随失败保存，但本轮不能声明处理了输入或产生了结果。
func validSessionFailure(event workspace.RunEvent) bool {
	return event.Kind == "failed" && (event.SessionFailure == "busy" || event.SessionFailure == "unavailable") &&
		event.InputReceipt == nil && event.Approval == nil && event.GitVerification == nil &&
		len(event.WaitForTaskIDs)+len(event.TransferIDs)+len(event.Artifacts) == 0 &&
		(event.Diagnostics == nil || len(event.Diagnostics.Reviews) == 0)
}

// settleSessionFailure 结算当前 dsh 续接轮，保留原运行凭据供迟到回执审计。
// started 只确认进程启动。已采纳会话或发生业务操作时须由原轮核对。
func settleSessionFailure(ctx context.Context, tx pgx.Tx, run runState, id, sessionID string, event workspace.RunEvent) error {
	if run.CurrentRun != id || run.AgentKind != "dsh" || run.InputSession == "" || run.InputSession != sessionID || event.Generation == 0 {
		return workspace.ErrConflict
	}
	var nativeWork bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM muster_run_events WHERE run_id=$1 AND payload->>'kind'='session') OR
		EXISTS (SELECT 1 FROM muster_git_push_intents WHERE run_id=$1) OR
		EXISTS (SELECT 1 FROM muster_transfers WHERE run_id=$1 AND source_device_id<>target_device_id) OR
		EXISTS (SELECT 1 FROM muster_delegations WHERE parent_run_id=$1)`, id).Scan(&nativeWork); err != nil {
		return err
	}
	if nativeWork {
		return workspace.ErrConflict
	}
	// 进程启动曾登记输入已送达。续接未采纳会话时恢复此前有效的送达记录。
	if _, err := tx.Exec(ctx, `UPDATE muster_task_inputs i SET delivered_run_id=ri.previous_delivery_run_id
		FROM muster_run_inputs ri WHERE ri.run_id=$1 AND (i.task_id,i.id)=(ri.task_id,ri.input_id)
		AND i.delivered_run_id=$1 AND i.handled_run_id IS NULL`, id); err != nil {
		return err
	}
	if run.ControlMode == "cancel" {
		return settleStoppedRun(ctx, tx, run, id, sessionID, event)
	}
	status, reason := "failed", "原生会话正被占用，稍后在原设备重试。"
	if event.SessionFailure == "unavailable" {
		status, reason = "interrupted", "原生会话损坏或不兼容，请核对运行记录后选择重新接手。"
	}
	if event.Text == "" {
		event.Text = reason
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_runs SET status=$2,session_id=$3,outcome=$4,
		diagnostics=$5,updated_at=clock_timestamp() WHERE id=$1`, id, status, sessionID, event.Text, event.Diagnostics); err != nil {
		return err
	}
	if event.SessionFailure == "busy" {
		if err := queueSessionRetry(ctx, tx, run, id); err != nil {
			return err
		}
	} else {
		if err := interruptUnavailableSession(ctx, tx, run, id, sessionID, reason); err != nil {
			return err
		}
	}
	if run.ParentTaskID != "" && run.ParentDeviceID == "" {
		if err := reconcileCenterTaskGroup(ctx, tx, run.ParentTaskID); err != nil {
			return err
		}
	}
	return advanceWorkspace(ctx, tx)
}

// queueSessionRetry 为原设备新建延迟轮次。原轮已经退出，后续回执不能改变新轮状态。
// 同一个输入冻结点和审批答复随续接重投；尚未进入本轮的追加要求继续排队。
func queueSessionRetry(ctx context.Context, tx pgx.Tx, run runState, previousID string) error {
	retryID := "run-" + stableUUID(previousID+"\x00session-retry")
	if _, err := tx.Exec(ctx, `INSERT INTO muster_runs (id,task_id,input,device_id,generation,prompt_transfer_id,
		ready_priority,dependency_results,input_session,input_receipts_frozen,input_receipt_supported,
		session_retry_after,session_retry_origin)
		SELECT $1,r.task_id,r.input,r.device_id,(SELECT MAX(generation)+1 FROM muster_runs WHERE task_id=r.task_id),
			r.prompt_transfer_id,r.ready_priority,r.dependency_results,r.input_session,r.input_receipts_frozen,r.input_receipt_supported,
			clock_timestamp()+interval '30 seconds',COALESCE(r.session_retry_origin,r.id)
		FROM muster_runs r WHERE r.id=$2`, retryID, previousID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO muster_run_inputs(run_id,task_id,input_id)
		SELECT $1,task_id,input_id FROM muster_run_inputs WHERE run_id=$2`, retryID, previousID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO muster_run_transfers(run_id,transfer_id,direction)
		SELECT $1,transfer_id,direction FROM muster_run_transfers WHERE run_id=$2 AND direction='input'`, retryID, previousID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_approvals SET resume_run_id=$1
		WHERE resume_run_id=$2 AND handled_at IS NULL AND cancelled_at IS NULL`, retryID, previousID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE muster_tasks SET status='queued',current_run_id=$2,
		control_mode=CASE WHEN control_mode='stop' THEN 'pause' ELSE control_mode END,
		result='',updated_at=clock_timestamp() WHERE id=$1`, run.TaskID, retryID)
	return err
}

// interruptUnavailableSession 保留原会话引用并冻结待执行轮次，恢复须由用户明确发起。
func interruptUnavailableSession(ctx context.Context, tx pgx.Tx, run runState, id, sessionID, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE muster_runs SET status='interrupted',updated_at=clock_timestamp()
		WHERE task_id=$1 AND id<>$2 AND status='queued'`, run.TaskID, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_tasks SET status='interrupted',session_id=$2,result=$3,
		waiting_reason='session_unavailable',control_mode='',updated_at=clock_timestamp() WHERE id=$1`, run.TaskID, sessionID, reason); err != nil {
		return err
	}
	// 已派发的追加工作保留任务输入和运行。尚待插件处理的消息结束投递，避免阻塞恢复汇报。
	if _, err := tx.Exec(ctx, `WITH closed AS (
		UPDATE muster_deliveries SET done=true WHERE kind='message' AND NOT done AND task_id=$1
		RETURNING message_id
	) UPDATE muster_messages SET status='interrupted' WHERE status='accepted' AND id IN (SELECT message_id FROM closed)`, run.TaskID); err != nil {
		return err
	}
	if run.ParentTaskID == "" {
		return enqueueTaskReport(ctx, tx, "report-"+id, run.TaskID, "运行状态", "interrupted", reason)
	}
	if run.ParentDeviceID != "" {
		return reconcileParent(ctx, tx, run.TaskID)
	}
	return nil
}
