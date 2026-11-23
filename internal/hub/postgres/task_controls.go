package postgres

import (
	"context"
	"errors"
	"slices"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

type taskControlState struct {
	status, mode      string
	active, supported bool
}

func (state taskControlState) actions() []string {
	switch state.status {
	case "completed", "failed", "cancelled":
		return nil
	case "interrupted":
		if state.mode == "pause" {
			return []string{"cancel"}
		}
		return nil
	}
	switch state.mode {
	case "cancel":
		return nil
	case "stop":
		return []string{"cancel"}
	case "pause":
		if state.active && state.supported {
			return []string{"resume", "stop_current", "cancel"}
		}
		return []string{"resume", "cancel"}
	default:
		if state.active && state.supported {
			return []string{"pause", "stop_current", "cancel"}
		}
		return []string{"pause", "cancel"}
	}
}

func (state taskControlState) displayStatus(status string) string {
	if status == "cancelled" {
		return status
	}
	switch state.mode {
	case "cancel":
		return "cancelling"
	case "stop":
		return "stopping"
	case "pause":
		if status == "interrupted" {
			return status
		}
		if state.active {
			return "pausing"
		}
		return "paused"
	default:
		return status
	}
}

// ControlTask 在同一事务记录操作和工作区游标。重复请求只读取当前状态。
func (store *InventoryStore) ControlTask(ctx context.Context, id string, input workspace.TaskControl) (task workspace.Task, err error) {
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		var previous string
		err := tx.QueryRow(ctx, `SELECT action FROM muster_task_control_requests
			WHERE task_id=$1 AND request_id=$2::text::uuid`, id, input.RequestID).Scan(&previous)
		if err == nil && previous != input.Action {
			return workspace.ErrConflict
		}
		replay := err == nil
		if !replay && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		read := func() error {
			var readErr error
			task, readErr = scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM muster_tasks t WHERE t.id=$1", id))
			return notFound(readErr)
		}
		if err := read(); err != nil || replay {
			return err
		}
		if !slices.Contains(task.ControlActions, input.Action) {
			return workspace.ErrConflict
		}
		if err := applyTaskControl(ctx, tx, id, input); err != nil {
			return err
		}
		if input.Action != "cancel" {
			if err := reconcileCenterGroupForStep(ctx, tx, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO muster_task_control_requests(task_id,request_id,action)
			VALUES($1,$2::text::uuid,$3)`, id, input.RequestID, input.Action); err != nil {
			return err
		}
		if err := read(); err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
	return task, err
}

func applyTaskControl(ctx context.Context, tx pgx.Tx, id string, input workspace.TaskControl) error {
	var mode string
	switch input.Action {
	case "pause":
		mode = "pause"
	case "stop_current":
		mode = "stop"
	case "resume":
		if err := resumeStoppedRun(ctx, tx, id, input.RequestID); err != nil {
			return err
		}
	case "cancel":
		return cancelTaskTree(ctx, tx, id)
	}
	_, err := tx.Exec(ctx, `UPDATE muster_tasks SET control_mode=$2,updated_at=clock_timestamp() WHERE id=$1`, id, mode)
	return err
}

// RunControl 返回当前轮次的停止指令。已有 Git 推送意图时允许用原凭据在过期后查询，供远端回读恢复。
func (store *InventoryStore) RunControl(ctx context.Context, device string, token [32]byte, id string, generation int64, lease string) (stop bool, err error) {
	err = pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		run, err := readRunState(ctx, tx, device, id)
		if err != nil {
			return err
		}
		if !run.ControlSupported || run.CurrentRun != id || lease == "" || run.Generation != generation ||
			run.Lease != lease || (run.Status != "queued" && run.Status != "running") {
			return workspace.ErrConflict
		}
		if !run.LeaseLive {
			if !run.GitPublish {
				return workspace.ErrConflict
			}
			var intentExists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM muster_git_push_intents WHERE run_id=$1)", id).Scan(&intentExists); err != nil {
				return err
			}
			if !intentExists {
				return workspace.ErrConflict
			}
		}
		stop = run.stopping()
		return nil
	})
	return stop, err
}

func (run runState) stopping() bool {
	return run.ControlMode == "stop" || run.ControlMode == "cancel"
}

// settleStoppedRun 只在已收到当前轮的终止回执后释放设备名额。
// 若 Hub 已确认启动且无会话标识，保留现场供核对，不能自动重开 CLI。
func settleStoppedRun(ctx context.Context, tx pgx.Tx, run runState, id, sessionID string, event workspace.RunEvent) error {
	runStatus := "stopped"
	if run.ControlMode == "cancel" {
		// 已自然退出的轮次仍显示其原始退出结果，包括旧 daemon 无法处理中止指令时的回执。
		runStatus = event.Kind
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_runs SET status=$2,session_id=$3,outcome=$4,diagnostics=$5,git_verification=$6,updated_at=clock_timestamp()
		WHERE id=$1`, id, runStatus, sessionID, event.Text, event.Diagnostics, event.GitVerification); err != nil {
		return err
	}
	if run.ControlMode == "cancel" {
		if err := settleCancelledTree(ctx, tx, run.RootTaskID); err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	}
	status, outcome := "paused", event.Text
	if run.Status == "running" && sessionID == "" {
		status, outcome = "interrupted", "本轮已停止，尚无可续接的原生会话，请核对工作现场。"
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_tasks SET status=$2,control_mode='pause',session_id=$3,
		result=$4,updated_at=clock_timestamp() WHERE id=$1`, run.TaskID, status, sessionID, outcome); err != nil {
		return err
	}
	if status == "interrupted" && run.ParentTaskID != "" {
		if err := reconcileParent(ctx, tx, run.TaskID); err != nil {
			return err
		}
	} else if run.ParentTaskID != "" && run.ParentDeviceID == "" {
		if err := reconcileCenterTaskGroup(ctx, tx, run.ParentTaskID); err != nil {
			return err
		}
	}
	return advanceWorkspace(ctx, tx)
}

// resumeStoppedRun 复制停止轮次的输入与附件，保留原运行记录。已确认在等待的操作只解除暂停。
func resumeStoppedRun(ctx context.Context, tx pgx.Tx, taskID, requestID string) error {
	var prior, prompt, transfer, session string
	var priority int
	var trackedMessage bool
	err := tx.QueryRow(ctx, `SELECT r.id,r.input,r.prompt_transfer_id,r.ready_priority,t.session_id,
		EXISTS(SELECT 1 FROM muster_task_inputs i WHERE i.task_id=t.id AND i.origin_run_id=r.id AND i.message_id IS NOT NULL)
		FROM muster_tasks t JOIN muster_runs r ON r.id=t.current_run_id
		WHERE t.id=$1 AND t.status='paused' AND r.status='stopped'`, taskID).
		Scan(&prior, &prompt, &transfer, &priority, &session, &trackedMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if trackedMessage {
		// 追加要求由冻结输入清单重投。原提示词不得绕过已处理输入的过滤。
		prompt = "请核对上一轮停止后的工作现场，并继续当前任务。"
	}
	if session != "" {
		prompt = "上一轮已停止。请先核对当前文件、会话和已完成操作，再处理尚未完成的要求。\n\n" + prompt
	}
	// 显式恢复是用户操作。即使复制了依赖输入，也应先核对停止现场。
	if priority == priorityDependency {
		priority = priorityFollowUp
	}
	runID := manualRecoveryRunID(taskID, requestID)
	if err := nextRun(ctx, tx, taskID, runID, prompt, transfer, priority); err != nil {
		return err
	}
	// 被停止的轮次仍拥有尚未处理的输入，优先续接它，之后才处理排队的追加要求。
	if _, err := tx.Exec(ctx, `UPDATE muster_runs SET dependency_results=(SELECT dependency_results FROM muster_runs WHERE id=$2)
		WHERE id=$1`, runID, prior); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO muster_run_transfers(run_id,transfer_id,direction)
		SELECT $1,transfer_id,direction FROM muster_run_transfers WHERE run_id=$2 AND direction='input'`, runID, prior); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_approvals SET resume_run_id=$1 WHERE resume_run_id=$2 AND handled_at IS NULL`, runID, prior); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE muster_tasks SET status='queued',current_run_id=$2,result='' WHERE id=$1", taskID, runID)
	return err
}

// manualRecoveryRunID 从已持久化的恢复请求重建轮次 ID，供重试和历史回填使用。
func manualRecoveryRunID(taskID, requestID string) string {
	return "run-" + stableUUID(taskID+"\x00resume\x00"+requestID)
}

// migrateQueuedManualRecoveries 纠正旧版继承了依赖优先级的手动恢复轮次。
// 以恢复请求重建轮次 ID，仅提升仍为当前轮且尚未领取的匹配记录。
func migrateQueuedManualRecoveries(ctx context.Context, tx pgx.Tx) error {
	type recoveryRequest struct{ taskID, requestID, runID string }
	requests, err := selectRows(ctx, tx, `SELECT t.id,c.request_id::text,r.id
		FROM muster_runs r JOIN muster_tasks t ON t.id=r.task_id AND t.current_run_id=r.id
		JOIN muster_task_control_requests c ON c.task_id=t.id AND c.action='resume'
		WHERE r.status='queued' AND r.claimed_at IS NULL AND r.ready_priority=$1 AND r.dependency_results IS NOT NULL`,
		func(row pgx.Row) (request recoveryRequest, err error) {
			err = row.Scan(&request.taskID, &request.requestID, &request.runID)
			return
		}, priorityDependency)
	if err != nil {
		return err
	}
	var runIDs []string
	for _, request := range requests {
		if request.runID == manualRecoveryRunID(request.taskID, request.requestID) {
			runIDs = append(runIDs, request.runID)
		}
	}
	if len(runIDs) == 0 {
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE muster_runs r SET ready_priority=$2
		FROM muster_tasks t WHERE t.id=r.task_id AND t.current_run_id=r.id
		AND r.id=ANY($1::text[]) AND r.status='queued' AND r.claimed_at IS NULL AND r.ready_priority=$3`,
		runIDs, priorityFollowUp, priorityDependency)
	return err
}

// cancelTaskTree 冻结子树的派发和请示，已领取轮次继续等待设备确认退出。
func cancelTaskTree(ctx context.Context, tx pgx.Tx, id string) error {
	var root string
	if err := tx.QueryRow(ctx, "SELECT root_id FROM muster_tasks WHERE id=$1", id).Scan(&root); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `WITH RECURSIVE subtree(id) AS (
		SELECT id FROM muster_tasks WHERE id=$1
		UNION ALL SELECT t.id FROM muster_tasks t JOIN subtree s ON t.parent_id=s.id
	)
	UPDATE muster_tasks SET control_mode='cancel',updated_at=clock_timestamp()
	WHERE id IN (SELECT id FROM subtree) AND status NOT IN ('completed','failed','cancelled')`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_runs r SET status='cancelled',updated_at=clock_timestamp()
		FROM muster_tasks t WHERE t.id=r.task_id AND t.root_id=$1 AND t.control_mode='cancel'
		AND r.claimed_at IS NULL AND r.status='queued'`, root); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `WITH cancelled AS (
		UPDATE muster_approvals a SET cancelled_at=clock_timestamp() FROM muster_tasks t
		WHERE t.id=a.task_id AND t.root_id=$1 AND t.control_mode='cancel'
		AND a.handled_at IS NULL AND a.cancelled_at IS NULL RETURNING a.id
	)
	UPDATE muster_approval_notifications SET desired_status='cancelled',next_attempt_at=clock_timestamp()
	WHERE approval_id IN (SELECT id FROM cancelled)`, root); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `WITH discarded AS (
		UPDATE muster_deliveries d SET done=true FROM muster_tasks t
		WHERE d.task_id=t.id AND t.root_id=$1 AND t.control_mode='cancel' AND d.kind='message'
		AND NOT d.done AND d.dispatch_task_id IS NULL RETURNING d.message_id
	)
	UPDATE muster_messages SET status='interrupted' WHERE id IN (SELECT message_id FROM discarded) AND status='accepted'`, root); err != nil {
		return err
	}
	return settleCancelledTree(ctx, tx, root)
}

// settleCancelledTree 等所有已领取的后代退出后才将相应祖先标为已取消。
func settleCancelledTree(ctx context.Context, tx pgx.Tx, root string) error {
	ids, err := selectRows(ctx, tx, `WITH RECURSIVE outstanding(id,parent_id) AS (
		SELECT t.id,t.parent_id FROM muster_tasks t JOIN muster_runs r ON r.task_id=t.id
		WHERE t.root_id=$1 AND t.control_mode='cancel' AND r.claimed_at IS NOT NULL AND r.status IN ('queued','running')
		UNION SELECT t.id,t.parent_id FROM muster_tasks t JOIN outstanding o ON t.id=o.parent_id
	)
	UPDATE muster_tasks SET status='cancelled',waiting_reason='',updated_at=clock_timestamp()
	WHERE root_id=$1 AND control_mode='cancel' AND status<>'cancelled' AND id NOT IN (SELECT id FROM outstanding)
	RETURNING id`, func(row pgx.Row) (id string, err error) {
		err = row.Scan(&id)
		return
	}, root)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if id == root {
			if err := reconcileCenterTaskGroup(ctx, tx, root); err != nil {
				return err
			}
			continue
		}
		if err := reconcileParent(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}
