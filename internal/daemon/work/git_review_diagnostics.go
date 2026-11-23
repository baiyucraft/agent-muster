package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

// saveGitReview 同步记录审查的归属和最后确认的状态。启动前先写入未确认状态，
// 进程退出及候选重新核对后再更新，使设备重启可以恢复已有证据。
func (worker *Worker) saveGitReview(ctx context.Context, runID string, attempt runreport.ReviewAttempt) error {
	if !attempt.Valid() {
		return errors.New("Git 独立 Review 诊断格式无效")
	}
	data, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err = worker.db.ExecContext(persist, `INSERT INTO git_review_attempts (run_id, repair_index, data) VALUES (?,?,?)
		ON CONFLICT (run_id, repair_index) DO UPDATE SET data=excluded.data`, runID, attempt.RepairIndex, data)
	return err
}

// collectGitReviews 在写终态 outbox 的同一事务中转入审查证据。正常完成、中断和推送读回共用该路径。
func collectGitReviews(ctx context.Context, tx *sql.Tx, runID string, event *control.RunEvent) error {
	rows, err := tx.QueryContext(ctx, `SELECT data FROM git_review_attempts WHERE run_id=? ORDER BY repair_index`, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var reviews []runreport.ReviewAttempt
	for rows.Next() {
		var data []byte
		var attempt runreport.ReviewAttempt
		if err := rows.Scan(&data); err != nil {
			return err
		}
		if err := json.Unmarshal(data, &attempt); err != nil {
			return err
		}
		reviews = append(reviews, attempt)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(reviews) == 0 {
		return nil
	}
	report := runreport.Diagnostics{Reviews: reviews}
	if event.Diagnostics != nil {
		report = *event.Diagnostics
		report.Reviews = reviews
	}
	if !report.Valid() {
		return errors.New("Git 独立 Review 诊断格式无效")
	}
	event.Diagnostics = &report
	_, err = tx.ExecContext(ctx, `DELETE FROM git_review_attempts WHERE run_id=?`, runID)
	return err
}
