package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

func stableUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	sum[6] = (sum[6] & 0x0f) | 0x80
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// Delegate 用父运行和请求键去重，同一摘要的重试返回已创建的子任务。
// 首次委派要求有效的当前运行租约，并为跨设备子任务绑定输入投递。
func (store *InventoryStore) Delegate(ctx context.Context, device string, token [32]byte, runID string, input workspace.Delegation) (task workspace.Task, err error) {
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		// Normalize the transfer list so replaying a request can resolve the same durable task.
		input.TransferIDs = append([]string(nil), input.TransferIDs...)
		sort.Strings(input.TransferIDs)
		if input.TaskID == "" {
			input.TaskID = "task-" + stableUUID(runID+"\x00"+input.RequestKey)
		}
		digestInput := input
		digestInput.Lease = ""
		payload, err := json.Marshal(digestInput)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(payload)
		var prior string
		var priorDigest []byte
		run, err := readRunState(ctx, tx, device, runID)
		if err != nil {
			return err
		}
		if run.Lease != input.Lease || input.Lease == "" || run.Generation != input.Generation {
			return workspace.ErrConflict
		}
		err = tx.QueryRow(ctx, `SELECT child_task_id,request_digest FROM muster_delegations
			WHERE parent_run_id=$1 AND request_key=$2`, runID, input.RequestKey).Scan(&prior, &priorDigest)
		if err == nil {
			if !bytes.Equal(digest[:], priorDigest) {
				return workspace.ErrConflict
			}
			task, err = scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM muster_tasks t WHERE t.id=$1", prior))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if run.CurrentRun != runID || run.Status != "running" || !run.LeaseLive || run.stopping() {
			return workspace.ErrConflict
		}
		route, err := resolveBinding(ctx, tx, input.ProjectID, input.BindingID)
		if err != nil {
			return err
		}
		if route.DeviceID == device {
			return workspace.ErrConflict
		}
		childRunID := "run-" + strings.TrimPrefix(input.TaskID, "task-")
		if err := insertTask(ctx, tx, input.TaskID, childRunID, input.Title, run.TaskID, run.RootTaskID, route); err != nil {
			return err
		}
		if err := nextRun(ctx, tx, input.TaskID, childRunID, "", input.PromptTransferID, priorityNew); err != nil {
			return err
		}
		if err := linkTransfer(ctx, tx, input.PromptTransferID, runID, childRunID, route.DeviceID, "input", "prompt"); err != nil {
			return err
		}
		for _, id := range input.TransferIDs {
			if err := linkTransfer(ctx, tx, id, runID, childRunID, route.DeviceID, "input", "file", "message"); err != nil {
				return err
			}
		}
		if err := registerRunTransfers(ctx, tx, childRunID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO muster_delegations(parent_run_id,request_key,child_task_id,request_digest)
			VALUES($1,$2,$3,$4)`, runID, input.RequestKey, input.TaskID, digest[:]); err != nil {
			return err
		}
		if err := queueReadyChild(ctx, tx, childRunID); err != nil {
			return err
		}
		task, err = scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM muster_tasks t WHERE t.id=$1", input.TaskID))
		if err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
	return
}

func linkTransfer(ctx context.Context, tx pgx.Tx, id, sourceRun, targetRun, targetDevice, direction string, kinds ...string) error {
	tag, err := tx.Exec(ctx, `INSERT INTO muster_run_transfers(run_id,transfer_id,direction)
		SELECT $3,tr.id,$6 FROM muster_transfers tr WHERE tr.id=$1 AND tr.run_id=$2 AND tr.target_device_id=$4
		AND tr.kind=ANY($5::text[]) ON CONFLICT DO NOTHING`, id, sourceRun, targetRun, targetDevice, kinds, direction)
	if err == nil && tag.RowsAffected() == 0 {
		return workspace.ErrConflict
	}
	return err
}

func queueReadyChild(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx, `UPDATE muster_tasks t SET status='queued',waiting_reason='',updated_at=clock_timestamp()
		WHERE t.current_run_id=$1 AND t.status='waiting' AND t.waiting_reason='delivery'
		AND NOT EXISTS (SELECT 1 FROM muster_run_transfers rt JOIN muster_transfers tr ON tr.id=rt.transfer_id
			WHERE rt.run_id=$1 AND rt.direction='input' AND tr.received_at IS NULL)`, runID)
	return err
}

func reconcileInputTransfer(ctx context.Context, tx pgx.Tx, transferID string) error {
	runs, err := selectRows(ctx, tx, `SELECT run_id FROM muster_run_transfers WHERE transfer_id=$1 AND direction='input'`,
		func(row pgx.Row) (string, error) { var id string; err := row.Scan(&id); return id, err }, transferID)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if err := queueReadyChild(ctx, tx, run); err != nil {
			return err
		}
	}
	return nil
}

// ValidateWait 校验当前运行的租约、运行代次和等待对象的归属。
func (store *InventoryStore) ValidateWait(ctx context.Context, device string, token [32]byte, runID string, event workspace.RunEvent) error {
	return pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		run, err := readRunState(ctx, tx, device, runID)
		if err != nil {
			return err
		}
		if run.CurrentRun != runID || run.Status != "running" || !run.LeaseLive || run.stopping() ||
			event.Lease == "" || event.Lease != run.Lease || event.Generation != run.Generation {
			return workspace.ErrConflict
		}
		return validateWaitReferences(ctx, tx, run, runID, device, event.WaitForTaskIDs, event.TransferIDs)
	})
}

func validateWaitReferences(ctx context.Context, tx pgx.Tx, run runState, runID, device string, children, transfers []string) error {
	if len(children)+len(transfers) == 0 {
		return workspace.ErrConflict
	}
	var childCount, transferCount int
	err := tx.QueryRow(ctx, `
		SELECT (
			SELECT count(*) FROM muster_tasks t WHERE t.id=ANY($1::text[]) AND t.parent_id=$3 AND EXISTS (
				SELECT 1 FROM muster_delegations d WHERE d.child_task_id=t.id AND d.parent_run_id=$4
			)
		), (
			SELECT count(*) FROM muster_transfers tr JOIN muster_tasks t ON t.id=tr.task_id
			WHERE tr.id=ANY($2::text[]) AND
				((tr.target_device_id=$5 AND t.root_id=$6 AND tr.kind<>'prompt')
				OR (tr.source_device_id=$5 AND tr.task_id=$3))
		)`, children, transfers, run.TaskID, runID, device, run.RootTaskID).Scan(&childCount, &transferCount)
	if err != nil {
		return err
	}
	if childCount != len(children) || transferCount != len(transfers) {
		return workspace.ErrConflict
	}
	return nil
}

func recordWait(ctx context.Context, tx pgx.Tx, run runState, runID, device string, children, transfers []string) error {
	if err := validateWaitReferences(ctx, tx, run, runID, device, children, transfers); err != nil {
		return err
	}
	if err := supersedeWaitDependencies(ctx, tx, run.TaskID); err != nil {
		return err
	}
	// 追加轮声明了新的等待条件时，旧代次已结算但尚未领取的续接不再有效。
	if _, err := tx.Exec(ctx, `UPDATE muster_runs SET status='cancelled',updated_at=clock_timestamp()
		WHERE task_id=$1 AND status='queued' AND claimed_at IS NULL AND ready_priority=$2
			AND dependency_results IS NOT NULL`, run.TaskID, priorityDependency); err != nil {
		return err
	}
	var generation int64
	if err := tx.QueryRow(ctx, `UPDATE muster_tasks SET input_generation=input_generation+1 WHERE id=$1 RETURNING input_generation`,
		run.TaskID).Scan(&generation); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO muster_task_dependencies(task_id,depends_on_task_id,generation)
		SELECT $1,child,$2 FROM unnest($3::text[]) AS child`, run.TaskID, generation, children); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO muster_wait_transfers(task_id,generation,transfer_id)
		SELECT $1,$2,transfer FROM unnest($3::text[]) AS transfer`, run.TaskID, generation, transfers)
	return err
}

func recordResultTransfers(ctx context.Context, tx pgx.Tx, run runState, id string, transferIDs []string) error {
	if run.ParentDeviceID == "" && len(transferIDs) > 0 {
		return workspace.ErrConflict
	}
	for _, transferID := range transferIDs {
		if err := linkTransfer(ctx, tx, transferID, id, id, run.ParentDeviceID, "result", "result", "file", "message"); err != nil {
			return err
		}
	}
	return nil
}

func supersedeWaitDependencies(ctx context.Context, tx pgx.Tx, taskID string) error {
	_, err := tx.Exec(ctx, `UPDATE muster_task_dependencies d SET status='superseded' FROM muster_tasks t
		WHERE d.task_id=t.id AND t.id=$1 AND d.generation=t.input_generation AND d.status IN ('waiting','delivering')`, taskID)
	return err
}

func reconcileParent(ctx context.Context, tx pgx.Tx, childID string) error {
	var parentID string
	var centerGroup bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(t.parent_id,''),COALESCE(p.center_group,false)
		FROM muster_tasks t LEFT JOIN muster_tasks p ON p.id=t.parent_id WHERE t.id=$1`, childID).Scan(&parentID, &centerGroup); err != nil {
		return err
	}
	if parentID == "" {
		return nil
	}
	if centerGroup {
		return reconcileCenterTaskGroup(ctx, tx, parentID)
	}
	return reconcileWait(ctx, tx, parentID)
}

func reconcileTransferWaiters(ctx context.Context, tx pgx.Tx, transferID string) error {
	tasks, err := selectRows(ctx, tx, `SELECT w.task_id FROM muster_wait_transfers w
		JOIN muster_tasks t ON w.task_id=t.id AND w.generation=t.input_generation
		WHERE w.transfer_id=$1 AND t.waiting_reason IN ('children','transfers')`, func(row pgx.Row) (string, error) {
		var id string
		err := row.Scan(&id)
		return id, err
	}, transferID)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if err := reconcileWait(ctx, tx, task); err != nil {
			return err
		}
	}
	return nil
}

func reconcileWait(ctx context.Context, tx pgx.Tx, taskID string) error {
	var generation int64
	var device, status string
	err := tx.QueryRow(ctx, `SELECT input_generation,device_id,status FROM muster_tasks
		WHERE id=$1 AND control_mode<>'cancel' AND waiting_reason IN ('children','transfers')`, taskID).Scan(&generation, &device, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_task_dependencies d SET status=CASE
		WHEN c.status IN ('completed','failed','interrupted') AND EXISTS (
			SELECT 1 FROM muster_run_transfers rt JOIN muster_transfers tr ON tr.id=rt.transfer_id
			WHERE rt.run_id=c.current_run_id AND rt.direction='result' AND tr.received_at IS NULL
		) THEN 'delivering'
		WHEN c.status IN ('completed','failed','interrupted','cancelled') THEN c.status
		ELSE 'waiting' END
		FROM muster_tasks c WHERE c.id=d.depends_on_task_id AND d.task_id=$1 AND d.generation=$2`, taskID, generation); err != nil {
		return err
	}
	if status != "waiting" {
		return nil
	}
	dependencies, err := selectRows(ctx, tx, `SELECT depends_on_task_id,status FROM muster_task_dependencies
		WHERE task_id=$1 AND generation=$2 ORDER BY depends_on_task_id`, func(row pgx.Row) (workspace.Dependency, error) {
		var d workspace.Dependency
		err := row.Scan(&d.DependsOnTaskID, &d.Status)
		return d, err
	}, taskID, generation)
	if err != nil {
		return err
	}
	var expected, pending int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*),COUNT(*) FILTER (WHERE tr.received_at IS NULL)
		FROM muster_wait_transfers w JOIN muster_transfers tr ON w.transfer_id=tr.id
		WHERE w.task_id=$1 AND w.generation=$2`, taskID, generation).Scan(&expected, &pending); err != nil {
		return err
	}
	if pending != 0 || expected+len(dependencies) == 0 {
		return nil
	}
	var prompt strings.Builder
	results := make([]workspace.DependencyResult, 0, len(dependencies))
	prompt.WriteString("等待条件已满足。请根据以下状态和本地收到的交接材料继续当前任务。\n")
	for _, dependency := range dependencies {
		if dependency.Status == "waiting" || dependency.Status == "delivering" {
			return nil
		}
		fmt.Fprintf(&prompt, "%s %s\n", dependency.DependsOnTaskID, dependency.Status)
		results = append(results, workspace.DependencyResult{TaskID: dependency.DependsOnTaskID, Status: dependency.Status})
	}
	runID := "run-" + stableUUID(fmt.Sprintf("%s\x00wait\x00%d", taskID, generation))
	if err := nextRun(ctx, tx, taskID, runID, prompt.String(), "", priorityDependency); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE muster_runs SET dependency_results=$2 WHERE id=$1", runID, results); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO muster_run_transfers(run_id,transfer_id,direction)
		SELECT $1,tr.id,'input' FROM muster_task_dependencies d
		JOIN muster_tasks c ON c.id=d.depends_on_task_id
		JOIN muster_run_transfers rt ON rt.run_id=c.current_run_id AND rt.direction='result'
		JOIN muster_transfers tr ON tr.id=rt.transfer_id AND tr.target_device_id=$4 AND tr.received_at IS NOT NULL
		WHERE d.task_id=$2 AND d.generation=$3 ON CONFLICT DO NOTHING`, runID, taskID, generation, device); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO muster_run_transfers(run_id,transfer_id,direction)
		SELECT $1,w.transfer_id,'input' FROM muster_wait_transfers w
		JOIN muster_transfers tr ON tr.id=w.transfer_id AND tr.target_device_id=$4
		WHERE w.task_id=$2 AND w.generation=$3 ON CONFLICT DO NOTHING`, runID, taskID, generation, device); err != nil {
		return err
	}
	if err := registerRunTransfers(ctx, tx, runID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE muster_tasks SET status='queued',waiting_reason='',current_run_id=$2,updated_at=clock_timestamp()
		WHERE id=$1`, taskID, runID)
	if err != nil {
		return err
	}
	return reconcileCenterGroupForStep(ctx, tx, taskID)
}
