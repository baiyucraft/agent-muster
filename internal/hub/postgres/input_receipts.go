package postgres

import (
	"context"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/inputreceipt"
	"github.com/jackc/pgx/v5"
)

func registerRunTransfers(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO muster_task_inputs(task_id,id,origin_run_id,transfer_id)
		SELECT r.task_id,'transfer:' || tr.id,r.id,tr.id FROM muster_run_transfers rt
		JOIN muster_runs r ON r.id=rt.run_id JOIN muster_transfers tr ON tr.id=rt.transfer_id
		WHERE r.id=$1 AND rt.direction='input' AND tr.kind<>'prompt' AND tr.id<>r.prompt_transfer_id
		ON CONFLICT DO NOTHING`, runID)
	return err
}

// freezeRunInputs 按首领时的已知输入冻结本轮清单。排队中的其他轮次仍各自保留其新输入。
func freezeRunInputs(ctx context.Context, tx pgx.Tx, taskID, runID string) error {
	if err := registerRunTransfers(ctx, tx, runID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO muster_run_inputs(run_id,task_id,input_id)
		SELECT $2,i.task_id,i.id FROM muster_task_inputs i JOIN muster_runs r ON r.id=i.origin_run_id
		LEFT JOIN muster_transfers tr ON tr.id=i.transfer_id
		WHERE i.task_id=$1 AND i.handled_run_id IS NULL
		AND (i.transfer_id IS NULL OR tr.received_at IS NOT NULL)
		AND (r.id=$2 OR r.status NOT IN ('queued','running','cancelled') OR EXISTS (
			SELECT 1 FROM muster_run_transfers rt WHERE rt.run_id=$2 AND rt.transfer_id=i.transfer_id AND rt.direction='input'
		))`, taskID, runID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO muster_run_transfers(run_id,transfer_id,direction)
		SELECT $1,i.transfer_id,'input' FROM muster_run_inputs ri
		JOIN muster_task_inputs i ON (i.task_id,i.id)=(ri.task_id,ri.input_id)
		WHERE ri.run_id=$1 AND i.transfer_id IS NOT NULL ON CONFLICT DO NOTHING`, runID)
	return err
}

func loadClaimInputs(ctx context.Context, tx pgx.Tx, claim *workspace.RunClaim, supported bool) error {
	type runInput struct{ id, carriedPrompt string }
	inputs, err := selectRows(ctx, tx, `SELECT ri.input_id,
		CASE WHEN i.message_id IS NOT NULL AND i.origin_run_id<>$1 THEN origin.input ELSE '' END
		FROM muster_run_inputs ri JOIN muster_task_inputs i ON (i.task_id,i.id)=(ri.task_id,ri.input_id)
		JOIN muster_runs origin ON origin.id=i.origin_run_id WHERE ri.run_id=$1 ORDER BY origin.sequence,ri.input_id`,
		func(row pgx.Row) (item runInput, err error) {
			err = row.Scan(&item.id, &item.carriedPrompt)
			return
		}, claim.ID)
	if err != nil {
		return err
	}
	var legacyPrompt strings.Builder
	for _, item := range inputs {
		if supported {
			claim.InputIDs = append(claim.InputIDs, item.id)
			if item.carriedPrompt != "" {
				if claim.InputMessages == nil {
					claim.InputMessages = make(map[string]string)
				}
				claim.InputMessages[item.id] = item.carriedPrompt
			}
		} else if item.carriedPrompt != "" {
			legacyPrompt.WriteString(item.carriedPrompt)
			legacyPrompt.WriteString("\n\n")
		}
	}
	if legacyPrompt.Len() != 0 {
		claim.Prompt = "以下是尚待确认的追加要求：\n\n" + legacyPrompt.String() + claim.Prompt
	}
	return nil
}

func validateRunInputReceipt(ctx context.Context, tx pgx.Tx, runID string, receipt *inputreceipt.Receipt, requireAll bool) error {
	if receipt == nil && !requireAll {
		return nil
	}
	expected, err := selectRows(ctx, tx, `SELECT input_id FROM muster_run_inputs WHERE run_id=$1 ORDER BY input_id`,
		func(row pgx.Row) (id string, err error) {
			err = row.Scan(&id)
			return
		}, runID)
	if err != nil {
		return err
	}
	if !receipt.ValidFor(expected, requireAll) {
		return workspace.ErrInvalidInputReceipt
	}
	return nil
}

// requireStartedForInputReceipt 要求已收到设备启动回执。会话事件也能切换运行状态，但不能确认输入送达。
func requireStartedForInputReceipt(ctx context.Context, tx pgx.Tx, runID string, receipt *inputreceipt.Receipt) error {
	if receipt == nil || len(receipt.HandledIDs) == 0 {
		return nil
	}
	started, err := runStarted(ctx, tx, runID)
	if err != nil {
		return err
	}
	if !started {
		return workspace.ErrInvalidInputReceipt
	}
	return nil
}

// markRunInputsDelivered 只记录实际启动的当前轮次。领取和会话 ID 上报不足以确认投递。
func markRunInputsDelivered(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx, `UPDATE muster_task_inputs i SET delivered_run_id=$1 FROM muster_run_inputs ri
		WHERE ri.run_id=$1 AND (i.task_id,i.id)=(ri.task_id,ri.input_id) AND i.handled_run_id IS NULL`, runID)
	return err
}

// markRunInputsHandled 仅保存本轮终态明确确认且此前尚未处理的输入。
func markRunInputsHandled(ctx context.Context, tx pgx.Tx, runID string, receipt *inputreceipt.Receipt) error {
	if receipt == nil || len(receipt.HandledIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE muster_task_inputs i
		SET delivered_run_id=$1,handled_run_id=$1,handled_at=clock_timestamp() FROM muster_run_inputs ri
		WHERE ri.run_id=$1 AND (i.task_id,i.id)=(ri.task_id,ri.input_id)
		AND i.handled_run_id IS NULL AND i.id=ANY($2::text[])`, runID, receipt.HandledIDs)
	return err
}
