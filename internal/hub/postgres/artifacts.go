package postgres

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/baiyucraft/agent-muster/internal/artifact"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

const artifactColumns = "a.id,a.relative_path,a.sha256,a.size,a.task_id,a.run_id,a.device_id,a.created_at"

func scanArtifact(row pgx.Row) (item workspace.TaskArtifact, err error) {
	err = row.Scan(&item.ID, &item.Path, &item.SHA256, &item.Size, &item.TaskID, &item.RunID, &item.DeviceID, &item.CreatedAt)
	return
}

// Artifact 返回指定运行的已登记产物。调用方仍须验证任务归属和访问凭据。
func (store *InventoryStore) Artifact(ctx context.Context, runID, artifactID string) (workspace.TaskArtifact, error) {
	item, err := scanArtifact(store.pool.QueryRow(ctx, "SELECT "+artifactColumns+` FROM muster_run_artifacts a WHERE a.run_id=$1 AND a.id=$2`, runID, artifactID))
	return item, notFound(err)
}

func taskArtifacts(ctx context.Context, tx pgx.Tx, task workspace.Task) ([]workspace.TaskArtifact, error) {
	base := "SELECT " + artifactColumns + " FROM muster_run_artifacts a"
	const order = " ORDER BY a.created_at DESC,a.id"
	if task.RootTaskID == task.ID {
		return selectRows(ctx, tx, base+` JOIN muster_tasks t ON t.id=a.task_id WHERE t.root_id=$1`+order, scanArtifact, task.ID)
	}
	return selectRows(ctx, tx, base+` WHERE a.task_id=$1`+order, scanArtifact, task.ID)
}

func acceptsArtifacts(kind string) bool {
	switch kind {
	case "completed", "failed", "interrupted", "stopped", "approval_requested", "waiting":
		return true
	default:
		return false
	}
}

// recordRunArtifacts 将产物写入终态事务。迟到事件登记旧轮次证据，不加入后续汇报。
func recordRunArtifacts(ctx context.Context, tx pgx.Tx, run runState, runID, device string, sequence int64, refs []artifact.Reference) error {
	if len(refs) == 0 {
		return nil
	}
	started, err := runStarted(ctx, tx, runID)
	if err != nil {
		return err
	}
	if !started {
		return workspace.ErrInvalidArtifacts
	}
	auditOnly := run.CurrentRun != runID || run.Status != "queued" && run.Status != "running"
	for _, ref := range refs {
		tag, err := tx.Exec(ctx, `INSERT INTO muster_run_artifacts AS a
			(run_id,id,task_id,device_id,event_sequence,relative_path,sha256,size,audit_only)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (run_id,id) DO UPDATE SET size=a.size
			WHERE a.relative_path=EXCLUDED.relative_path AND a.sha256=EXCLUDED.sha256 AND a.size=EXCLUDED.size`,
			runID, ref.ID, run.TaskID, device, sequence, ref.Path, ref.SHA256, ref.Size, auditOnly)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return workspace.ErrConflict
		}
	}
	// 工作区写入串行化。按本轮唯一记录计数，超限时回滚整条事件及其产物。
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM muster_run_artifacts WHERE run_id=$1`, runID).Scan(&total); err != nil {
		return err
	}
	if total > artifact.MaxCount {
		return workspace.ErrInvalidArtifacts
	}
	return nil
}

// enqueueTaskReport 冻结终态结果和产物引用。重复结算沿用同一汇报 ID。
func enqueueTaskReport(ctx context.Context, tx pgx.Tx, reportID, taskID, statusLabel, status, outcome string) error {
	var title string
	if err := tx.QueryRow(ctx, `SELECT title FROM muster_tasks WHERE id=$1`, taskID).Scan(&title); err != nil {
		return err
	}
	result := fmt.Sprintf("%s\n%s: %s\n%s", title, statusLabel, status, outcome)
	body, err := taskReportBody(ctx, tx, taskID, result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO muster_deliveries(id,kind,body,task_id) VALUES($1,'report',$2,$3)
		ON CONFLICT (id) DO NOTHING`, reportID, body, taskID)
	return err
}

// taskReportBody 优先保留产物位置，再将正文限制在单次汇报的字节预算内。
func taskReportBody(ctx context.Context, tx pgx.Tx, rootID, result string) (string, error) {
	const (
		bodyLimit      = 12000
		resultBudget   = 2000
		footerTitle    = "\n\n已登记产物\n"
		footerOverflow = "\n其余产物见任务详情。"
	)
	rows, err := tx.Query(ctx, `SELECT a.device_id,a.relative_path,a.sha256 FROM muster_run_artifacts a
		JOIN muster_tasks t ON t.id=a.task_id WHERE t.root_id=$1 AND NOT a.audit_only
		ORDER BY a.created_at DESC,a.id`, rootID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var references strings.Builder
	for rows.Next() {
		var device, path, digest string
		if err := rows.Scan(&device, &path, &digest); err != nil {
			return "", err
		}
		line := fmt.Sprintf("- %s · %s · SHA-256 %s\n", device, path, digest)
		if len(line)+references.Len()+len(footerTitle)+len(footerOverflow) > bodyLimit-resultBudget {
			references.WriteString(footerOverflow)
			break
		}
		references.WriteString(line)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if references.Len() == 0 {
		return result, nil
	}
	footer := footerTitle + strings.TrimRight(references.String(), "\n")
	return boundReportResult(result, bodyLimit-len(footer)) + footer, nil
}

func boundReportResult(result string, limit int) string {
	if len(result) <= limit {
		return result
	}
	const suffix = "…"
	end := limit - len(suffix)
	for !utf8.RuneStart(result[end]) {
		end--
	}
	return result[:end] + suffix
}
