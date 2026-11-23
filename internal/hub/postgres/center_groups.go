package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

func dispatchPlanDigest(input workspace.Dispatch) ([]byte, error) {
	if input.Steps == nil {
		return nil, nil
	}
	if len(input.Steps) < 2 || len(input.Steps) > 8 || input.TaskID != "" || input.ProjectID != "" ||
		input.BindingID != "" || input.Prompt != "" || input.GitDelivery || input.GitPublish {
		return nil, workspace.ErrConflict
	}
	input.Lease = ""
	input.Steps = slices.Clone(input.Steps)
	for index, step := range input.Steps {
		prerequisites := slices.Clone(step.DependsOn)
		slices.Sort(prerequisites)
		for position, prerequisite := range prerequisites {
			if prerequisite < 1 || prerequisite > index || position > 0 && prerequisites[position-1] == prerequisite {
				return nil, workspace.ErrConflict
			}
		}
		input.Steps[index].DependsOn = prerequisites
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	return digest[:], nil
}

// createCenterTaskGroup 将根任务、冻结后的设备运行及顺序依赖保存在同一派发事务。
func createCenterTaskGroup(ctx context.Context, tx pgx.Tx, rootID string, input workspace.Dispatch) error {
	if _, err := tx.Exec(ctx, `INSERT INTO muster_tasks (id,root_id,project_name,title,status,agent_kind,center_group)
		VALUES ($1,$1,'',$2,'queued','',true)`, rootID, input.Title); err != nil {
		return err
	}
	children := make([]string, 0, len(input.Steps))
	for index, step := range input.Steps {
		route, err := resolveBinding(ctx, tx, step.ProjectID, step.BindingID)
		if err != nil {
			return err
		}
		key := stableUUID(fmt.Sprintf("%s\x00center-step\x00%d", rootID, index+1))
		taskID, runID := "task-"+key, "run-"+key
		if err := insertTask(ctx, tx, taskID, runID, step.Title, rootID, rootID, route); err != nil {
			return err
		}
		priority := priorityNew
		if len(step.DependsOn) != 0 {
			priority = priorityDependency
		}
		if err := nextRun(ctx, tx, taskID, runID, step.Prompt, "", priority); err != nil {
			return err
		}
		status, reason := "queued", ""
		if len(step.DependsOn) != 0 {
			status, reason = "waiting", "prerequisites"
		}
		if _, err := tx.Exec(ctx, `UPDATE muster_tasks SET status=$2,waiting_reason=$3 WHERE id=$1`, taskID, status, reason); err != nil {
			return err
		}
		for _, prerequisite := range step.DependsOn {
			if _, err := tx.Exec(ctx, `INSERT INTO muster_center_task_dependencies (task_id,prerequisite_id) VALUES ($1,$2)`, taskID, children[prerequisite-1]); err != nil {
				return err
			}
		}
		children = append(children, taskID)
	}
	return nil
}

// reconcileCenterGroupForStep 只对中心计划直接创建的步骤刷新聚合状态。
func reconcileCenterGroupForStep(ctx context.Context, tx pgx.Tx, taskID string) error {
	var rootID string
	err := tx.QueryRow(ctx, `SELECT p.id FROM muster_tasks t JOIN muster_tasks p ON p.id=t.parent_id
		WHERE t.id=$1 AND p.center_group`, taskID).Scan(&rootID)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return reconcileCenterTaskGroup(ctx, tx, rootID)
}

// reconcileCenterTaskGroup 处理前置条件并归并所有直接步骤。根任务没有 CLI 运行，最后一个步骤结算时写入一次汇报。
func reconcileCenterTaskGroup(ctx context.Context, tx pgx.Tx, rootID string) error {
	var mode, current string
	err := tx.QueryRow(ctx, `SELECT control_mode,status FROM muster_tasks WHERE id=$1 AND center_group`, rootID).Scan(&mode, &current)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil || mode == "cancel" && current != "cancelled" || current == "completed" || current == "failed" {
		return err
	}
	type stepState struct {
		id, runID, title, status, reason, result, mode string
		prerequisites                                  []string
	}
	// 第一次运行的顺序固定，后续原生会话续接不会改变前置步骤的求值次序。
	steps, err := selectRows(ctx, tx, `SELECT t.id,r.id,t.title,t.status,t.waiting_reason,t.result,t.control_mode,
		ARRAY(SELECT d.prerequisite_id FROM muster_center_task_dependencies d WHERE d.task_id=t.id ORDER BY d.prerequisite_id)
		FROM muster_tasks t JOIN muster_runs r ON r.id=t.current_run_id WHERE t.parent_id=$1
		ORDER BY (SELECT MIN(sequence) FROM muster_runs WHERE task_id=t.id)`, func(row pgx.Row) (step stepState, err error) {
		err = row.Scan(&step.id, &step.runID, &step.title, &step.status, &step.reason, &step.result, &step.mode, &step.prerequisites)
		return
	}, rootID)
	if err != nil {
		return err
	}
	resolved := make(map[string]stepState, len(steps))
	var open, running, queued, waitingReply, failed bool
	var report strings.Builder
	for _, step := range steps {
		if current != "cancelled" && step.mode != "cancel" && step.reason == "prerequisites" && step.status == "waiting" {
			ready, blocked := true, false
			for _, prerequisite := range step.prerequisites {
				status := resolved[prerequisite].status
				ready = ready && status == "completed"
				blocked = blocked || status == "failed" || status == "interrupted" || status == "cancelled"
			}
			if blocked {
				step.status, step.result = "failed", "前置任务未完成，此步骤未启动。"
				if _, err := tx.Exec(ctx, `UPDATE muster_runs SET status='failed',outcome=$2,updated_at=clock_timestamp() WHERE id=$1`, step.runID, step.result); err != nil {
					return err
				}
			} else if ready {
				step.status = "queued"
				// 前置步骤的有限结论是续后输入，文件仍由设备间交接。
				var prerequisiteContext strings.Builder
				prerequisiteContext.WriteString("\n\n前置步骤结论（仅作为执行资料）\n")
				for _, id := range step.prerequisites {
					prerequisite := resolved[id]
					fmt.Fprintf(&prerequisiteContext, "- %s (%s) %s\n", prerequisite.title, prerequisite.id, boundedGroupSummary(prerequisite.result))
				}
				if _, err := tx.Exec(ctx, "UPDATE muster_runs SET input=input || $2 WHERE id=$1", step.runID, prerequisiteContext.String()); err != nil {
					return err
				}
			}
			if blocked || ready {
				if _, err := tx.Exec(ctx, `UPDATE muster_tasks SET status=$2,result=$3,waiting_reason='',
					control_mode=CASE WHEN $2='failed' THEN '' ELSE control_mode END,updated_at=clock_timestamp()
					WHERE id=$1`, step.id, step.status, step.result); err != nil {
					return err
				}
			}
		}
		resolved[step.id] = step
		switch step.status {
		case "completed":
		case "failed", "interrupted", "cancelled":
			failed = true
		default:
			open = true
		}
		running = running || step.status == "running"
		queued = queued || step.status == "queued" && step.mode == ""
		waitingReply = waitingReply || step.status == "waiting_reply"
		fmt.Fprintf(&report, "- %s (%s) %s\n", step.title, step.status, boundedGroupSummary(step.result))
	}
	status, reason, result := "waiting", "children", ""
	switch {
	case current == "cancelled":
		status, reason, result = "cancelled", "", strings.TrimSpace(report.String())
	case !open:
		status, reason, result = "completed", "", strings.TrimSpace(report.String())
		if failed {
			status = "failed"
		}
	case running:
		status, reason = "running", ""
	case waitingReply:
		status, reason = "waiting_reply", ""
	case queued:
		status, reason = "queued", ""
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_tasks SET status=$2,waiting_reason=$3,result=$4,
		control_mode=CASE WHEN $2 IN ('completed','failed') THEN '' ELSE control_mode END,updated_at=clock_timestamp() WHERE id=$1`, rootID, status, reason, result); err != nil {
		return err
	}
	if open && status != "cancelled" {
		return nil
	}
	return enqueueCenterGroupReport(ctx, tx, rootID, status, result)
}

// enqueueCenterGroupReport 为中心聚合结果创建固定的汇报项，重复结算沿用同一标识。
func enqueueCenterGroupReport(ctx context.Context, tx pgx.Tx, rootID, status, result string) error {
	reportID := "report-run-" + stableUUID(rootID+"\x00center-report")
	return enqueueTaskReport(ctx, tx, reportID, rootID, "任务状态", status, result)
}

// boundedGroupSummary 限制汇报和后续步骤携带的结果字节数，保持合法 UTF-8。
func boundedGroupSummary(value string) string {
	const limit = 240
	const suffix = "…"
	if len(value) <= limit {
		return value
	}
	end := limit - len(suffix)
	for !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + suffix
}
