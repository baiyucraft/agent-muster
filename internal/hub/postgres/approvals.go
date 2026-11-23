package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

const approvalColumns = `a.id,a.task_id,t.root_id,a.run_id,a.question,a.options,a.impact,
	CASE WHEN a.cancelled_at IS NOT NULL THEN 'cancelled' WHEN a.handled_at IS NOT NULL THEN 'handled'
		WHEN a.answered_at IS NOT NULL THEN 'answered' ELSE 'pending' END,
	a.answer,a.created_at,a.answered_at,a.handled_at
	FROM muster_approvals a JOIN muster_tasks t ON t.id=a.task_id`

func scanApproval(row pgx.Row) (value workspace.Approval, err error) {
	err = row.Scan(&value.ID, &value.TaskID, &value.RootTaskID, &value.RunID, &value.Question, &value.Options,
		&value.Impact, &value.Status, &value.Answer, &value.CreatedAt, &value.AnsweredAt, &value.HandledAt)
	return value, err
}

// recordApproval 为已退出的当前运行创建一条请示并暂停任务调度。
// 依赖等待计划保留，答复轮次结束后仍可用当前等待代次判断依赖是否满足。
func (store *InventoryStore) recordApproval(ctx context.Context, tx pgx.Tx, run runState, runID, sessionID string, question approval.Question) error {
	if run.Status != "running" || run.CurrentRun != runID || sessionID == "" {
		return workspace.ErrConflict
	}
	id := "approval-" + stableUUID(runID)
	if _, err := tx.Exec(ctx, `
		WITH message AS (
			INSERT INTO muster_messages(id,kind,body,task_id)
			VALUES ($1,'approval',$2,$3) RETURNING created_at
		)
		INSERT INTO muster_approvals(id,task_id,run_id,question,options,impact,created_at)
		SELECT $1,$3,$4,$2,$5,$6,created_at FROM message`,
		id, question.Question, run.TaskID, runID, question.Options, question.Impact); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_tasks
		SET status='waiting_reply',session_id=$2,updated_at=clock_timestamp()
		WHERE id=$1`, run.TaskID, sessionID); err != nil {
		return err
	}
	if store.recipient.AppID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO muster_approval_notifications(approval_id,app_id,open_id,desired_status)
		VALUES ($1,$2,$3,'pending')`, id, store.recipient.AppID, store.recipient.OpenID)
	return err
}

// AnswerApproval 在同一事务内保存首个答复并安排原生会话续接。
// 同一请示的后续有效提交返回已保存的答复，不再生成运行或游标。
func (store *InventoryStore) AnswerApproval(ctx context.Context, id, answer string) (value workspace.Approval, err error) {
	answer, err = normalizeApprovalAnswer(answer)
	if err != nil {
		return value, err
	}
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		value, err = scanApproval(tx.QueryRow(ctx, "SELECT "+approvalColumns+" WHERE a.id=$1", id))
		if err != nil {
			return notFound(err)
		}
		return saveApprovalAnswer(ctx, tx, &value, answer)
	})
	return value, err
}

func normalizeApprovalAnswer(answer string) (string, error) {
	if len(answer) > 2000 || !utf8.ValidString(answer) || strings.ContainsRune(answer, 0) {
		return "", workspace.ErrInvalidApproval
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", workspace.ErrInvalidApproval
	}
	return answer, nil
}

// saveApprovalAnswer 由 Web 和 IM 共用，调用方须持有工作区写入锁并已验证答复。
func saveApprovalAnswer(ctx context.Context, tx pgx.Tx, value *workspace.Approval, answer string) error {
	if value.Status == "cancelled" {
		return workspace.ErrConflict
	}
	if value.AnsweredAt != nil {
		return nil
	}
	var active bool
	err := tx.QueryRow(ctx, `SELECT status='waiting_reply' AND current_run_id=$2 AND session_id<>''
		FROM muster_tasks WHERE id=$1`, value.TaskID, value.RunID).Scan(&active)
	if err != nil {
		return notFound(err)
	}
	if !active {
		return workspace.ErrConflict
	}
	input, err := json.Marshal(struct {
		ID       string `json:"approval_id"`
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}{ID: value.ID, Question: value.Question, Answer: answer})
	if err != nil {
		return err
	}
	runID := "run-" + stableUUID(value.ID)
	prompt := "用户已答复你在当前任务中提出的请示。请依据以下答复继续原任务。\n" + string(input)
	if err := nextRun(ctx, tx, value.TaskID, runID, prompt, "", priorityFollowUp); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_tasks
		SET status='queued',current_run_id=$2,updated_at=clock_timestamp() WHERE id=$1`, value.TaskID, runID); err != nil {
		return err
	}
	var answeredAt time.Time
	if err := tx.QueryRow(ctx, `UPDATE muster_approvals
			SET answer=$2,resume_run_id=$3,answered_at=clock_timestamp()
			WHERE id=$1 RETURNING answered_at`, value.ID, answer, runID).Scan(&answeredAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE muster_approval_notifications SET desired_status='answered',
		next_attempt_at=clock_timestamp() WHERE approval_id=$1`, value.ID); err != nil {
		return err
	}
	value.Answer, value.AnsweredAt, value.Status = answer, &answeredAt, "answered"
	return advanceWorkspace(ctx, tx)
}

// finishApproval 标记答复所在的运行成功处理了输入；失败或中断保留已答复状态。
func finishApproval(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx, `WITH handled AS (
		UPDATE muster_approvals SET handled_at=clock_timestamp()
		WHERE resume_run_id=$1 AND handled_at IS NULL AND cancelled_at IS NULL RETURNING id
	)
	UPDATE muster_approval_notifications SET desired_status='handled',next_attempt_at=clock_timestamp()
	WHERE approval_id IN (SELECT id FROM handled)`, runID)
	return err
}
