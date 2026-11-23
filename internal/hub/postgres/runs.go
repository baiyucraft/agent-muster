package postgres

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/baiyucraft/agent-muster/internal/artifact"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/inputreceipt"
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
	return store.ClaimRunWithControl(ctx, device, token, false)
}

// ClaimRunWithControl 记录 daemon 是否能确认停止，并保留已领取轮次的原协议能力。
func (store *InventoryStore) ClaimRunWithControl(ctx context.Context, device string, token [32]byte, supported bool) (claim workspace.RunClaim, err error) {
	return store.ClaimRunWithCapabilities(ctx, device, token, supported, false)
}

// ClaimRunWithCapabilities 为设备领取兼容的运行，托管 Git 任务只交给能验证候选的设备端。
func (store *InventoryStore) ClaimRunWithCapabilities(ctx context.Context, device string, token [32]byte, controlSupported, gitVerificationSupported bool) (claim workspace.RunClaim, err error) {
	return store.ClaimRunWithGitPublication(ctx, device, token, controlSupported, gitVerificationSupported, false)
}

// ClaimRunWithGitPublication 仅向同时支持运行控制、候选验证与推送回读的设备领取推送任务。
func (store *InventoryStore) ClaimRunWithGitPublication(ctx context.Context, device string, token [32]byte, controlSupported, gitVerificationSupported, gitPublishSupported bool) (claim workspace.RunClaim, err error) {
	return store.ClaimRunWithInputReceipts(ctx, device, token, controlSupported, gitVerificationSupported, gitPublishSupported, false)
}

// ClaimRunWithInputReceipts 协商轮次处理回执能力并在首次领取时冻结输入清单。
// 同一轮次再次领取时保留已协商的能力、输入和租约身份。
func (store *InventoryStore) ClaimRunWithInputReceipts(ctx context.Context, device string, token [32]byte, controlSupported, gitVerificationSupported, gitPublishSupported, inputReceiptSupported bool) (claim workspace.RunClaim, err error) {
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		var firstClaim, receiptNegotiated, inputsFrozen, freezeInputs bool
		// 先处理已受理的用户要求。已领取的运行保持归属，设备可继续按原租约恢复。
		err := tx.QueryRow(ctx, `
			WITH choice AS (
				SELECT r.id,t.session_id,(r.claimed_at IS NULL) AS first_claim,NOT r.input_receipts_frozen AS freeze_inputs FROM muster_runs r
				JOIN muster_tasks t ON r.id=t.current_run_id AND r.task_id=t.id
				WHERE r.device_id=$1 AND r.status IN ('queued','running') AND t.status IN ('queued','running')
				AND (r.claimed_at IS NOT NULL OR t.control_mode='')
				AND (r.session_retry_after IS NULL OR r.session_retry_after<=clock_timestamp())
				AND (r.claimed_at IS NOT NULL OR r.session_retry_after IS NOT NULL OR r.ready_priority<>$6 OR r.dependency_results IS NULL OR NOT EXISTS (
					SELECT 1 FROM muster_messages m WHERE m.requested_task_id=t.id AND m.status='accepted'
				))
				AND (NOT r.control_supported OR $3)
				AND (t.git_policy IS NULL OR $4)
				AND (NOT t.git_publish OR ($3 AND $4 AND $5))
				AND (COALESCE(t.git_policy->>'review_agent','')='' OR $5 OR (r.claimed_at IS NOT NULL AND NOT r.git_review_supported))
				AND (NOT r.input_receipt_supported OR $7)
				AND NOT EXISTS (SELECT 1 FROM muster_runs active WHERE active.device_id=$1
					AND active.id<>r.id AND active.status IN ('queued','running') AND active.claimed_at IS NOT NULL)
				ORDER BY (r.claimed_at IS NOT NULL) DESC,
					CASE WHEN r.created_at<=clock_timestamp()-interval '5 minutes' THEN 0 ELSE r.ready_priority END,
					r.sequence LIMIT 1
			)
			UPDATE muster_runs r SET claimed_at=COALESCE(r.claimed_at,clock_timestamp()),
				control_supported=CASE WHEN choice.first_claim THEN $3 ELSE r.control_supported END,
				git_review_supported=CASE WHEN choice.first_claim THEN $5 ELSE r.git_review_supported END,
				input_receipt_supported=CASE WHEN choice.first_claim AND r.session_retry_origin IS NULL THEN $7 ELSE r.input_receipt_supported END,
				input_receipts_frozen=r.input_receipts_frozen OR choice.first_claim,
				input_session=CASE WHEN choice.first_claim AND r.session_retry_origin IS NULL THEN choice.session_id ELSE r.input_session END,
				lease=COALESCE(NULLIF(r.lease,''),$2),lease_until=clock_timestamp()+interval '60 seconds'
			FROM choice,muster_tasks t LEFT JOIN muster_tasks parent ON parent.id=t.parent_id
			WHERE r.id=choice.id AND t.id=r.task_id
			RETURNING r.id,t.id,t.directory_id,r.input,r.input_session,t.agent_kind,t.project_id,r.generation,
				r.lease,r.lease_until,t.binding_id,COALESCE(t.parent_id,''),COALESCE(parent.device_id,''),t.root_id,
				r.prompt_transfer_id,r.dependency_results,choice.first_claim,r.control_supported,t.git_policy,t.git_publish,
				r.input_receipt_supported,r.input_receipts_frozen,choice.freeze_inputs`,
			device, rand.Text(), controlSupported, gitVerificationSupported, gitPublishSupported, priorityDependency, inputReceiptSupported).
			Scan(&claim.ID, &claim.TaskID, &claim.DirectoryID, &claim.Prompt, &claim.SessionID, &claim.AgentKind,
				&claim.ProjectID, &claim.Generation, &claim.Lease, &claim.LeaseUntil, &claim.BindingID, &claim.ParentTaskID,
				&claim.ParentDeviceID, &claim.RootTaskID, &claim.PromptTransferID, &claim.DependencyResults, &firstClaim, &claim.ControlSupported, &claim.GitPolicy, &claim.GitPublish,
				&receiptNegotiated, &inputsFrozen, &freezeInputs)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		claim.LeaseUntil = claim.LeaseUntil.UTC()
		claim.SessionFailureSupported = claim.AgentKind == "dsh" && claim.SessionID != ""
		claim.ApprovalSupported = true
		claim.DiagnosticsSupported = true
		claim.ReviewDiagnosticsSupported = true
		claim.ArtifactsSupported = claim.GitPolicy == nil
		if firstClaim && freezeInputs {
			if err := freezeRunInputs(ctx, tx, claim.TaskID, claim.ID); err != nil {
				return err
			}
		}
		if err := loadClaimInputs(ctx, tx, &claim, receiptNegotiated); err != nil {
			return err
		}
		claim.Attachments, err = selectRows(ctx, tx, "SELECT "+transferColumns+` FROM muster_run_transfers rt
			JOIN muster_transfers tr ON tr.id=rt.transfer_id WHERE rt.run_id=$1 AND rt.direction='input'
			AND (NOT $3 OR tr.kind='prompt' OR tr.id=$2 OR EXISTS (
				SELECT 1 FROM muster_run_inputs ri WHERE ri.run_id=$1 AND ri.input_id='transfer:' || tr.id
			))
			ORDER BY tr.kind,tr.id`, scanTransfer, claim.ID, claim.PromptTransferID, inputsFrozen)
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
		if err != nil {
			return err
		}
		fits, err := fitRunClaim(&claim)
		if err != nil || !firstClaim {
			return err
		}
		if !fits {
			if err := interruptOversizedClaim(ctx, tx, device, claim); err != nil {
				return err
			}
			claim = workspace.RunClaim{}
		}
		return advanceWorkspace(ctx, tx)
	})
	return
}

func fitRunClaim(claim *workspace.RunClaim) (bool, error) {
	payload, err := json.Marshal(claim)
	// Hub 的 JSON Encoder 会在响应末尾追加一个换行。
	if err == nil && len(payload)+1 > inputreceipt.MaxClaimBytes && len(claim.DelegationTargets) > 0 {
		// 目标建议可省略，输入标识、消息和附件必须保留。
		claim.DelegationTargets = nil
		payload, err = json.Marshal(claim)
	}
	if err != nil || len(payload)+1 > inputreceipt.MaxClaimBytes {
		return false, err
	}
	if len(claim.InputIDs) == 0 {
		return true, nil
	}
	receipt, err := json.Marshal(inputreceipt.Receipt{HandledIDs: claim.InputIDs})
	return len(receipt) <= inputreceipt.MaxReceiptBytes, err
}

func interruptOversizedClaim(ctx context.Context, tx pgx.Tx, device string, claim workspace.RunClaim) error {
	run, err := readRunState(ctx, tx, device, claim.ID)
	if err != nil {
		return err
	}
	const outcome = "待处理的输入超过单轮传输上限。请核对任务输入并分批重新交办。"
	// 领取尚未提交，设备没有获得此轮执行权。保留冻结输入供任务详情核对。
	if _, err := tx.Exec(ctx, `UPDATE muster_runs SET status='interrupted',claimed_at=NULL,lease='',lease_until=NULL,
		outcome=$2,updated_at=clock_timestamp() WHERE id=$1`, claim.ID, outcome); err != nil {
		return err
	}
	run.WaitingReason = ""
	if err := updateTaskFromRun(ctx, tx, run, claim.ID, "interrupted", claim.SessionID, outcome, true, false); err != nil {
		return err
	}
	if run.ParentTaskID != "" && run.ParentDeviceID == "" {
		return reconcileCenterTaskGroup(ctx, tx, run.ParentTaskID)
	}
	return nil
}

type runState struct {
	TaskID                string
	CurrentRun            string
	ParentTaskID          string
	ParentDeviceID        string
	RootTaskID            string
	Status                string
	WaitingReason         string
	SessionID             string
	InputSession          string
	AgentKind             string
	Generation            int64
	Lease                 string
	LeaseLive             bool
	LegacyEvents          bool
	ControlMode           string
	ControlSupported      bool
	ManagedGit            bool
	GitPublish            bool
	GitReviewSupported    bool
	InputReceiptSupported bool
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
		COALESCE(NULLIF(r.session_id,''),r.input_session),r.input_session,t.agent_kind,r.generation,r.lease,
		COALESCE(r.lease_until>=clock_timestamp(),false),r.legacy_events,t.control_mode,r.control_supported,t.git_policy IS NOT NULL,t.git_publish,r.git_review_supported,r.input_receipt_supported
		FROM muster_runs r JOIN muster_tasks t ON t.id=r.task_id LEFT JOIN muster_tasks p ON t.parent_id=p.id
		WHERE r.id=$1 AND r.device_id=$2 AND r.claimed_at IS NOT NULL`, id, device).
		Scan(&value.TaskID, &value.CurrentRun, &value.ParentTaskID, &value.ParentDeviceID, &value.RootTaskID,
			&value.Status, &value.WaitingReason, &value.SessionID, &value.InputSession, &value.AgentKind, &value.Generation, &value.Lease, &value.LeaseLive,
			&value.LegacyEvents, &value.ControlMode, &value.ControlSupported, &value.ManagedGit, &value.GitPublish, &value.GitReviewSupported, &value.InputReceiptSupported)
	return value, notFound(err)
}

// runStarted 只认可首个终态之前收到的启动事件。迟到启动留在审计记录中。
func runStarted(ctx context.Context, tx pgx.Tx, runID string) (started bool, err error) {
	err = tx.QueryRow(ctx, `SELECT COALESCE(
		MIN(sequence) FILTER (WHERE payload->>'kind'='started') <
		MIN(sequence) FILTER (WHERE payload->>'kind' NOT IN ('started','session')),
		BOOL_OR(payload->>'kind'='started'),false)
		FROM muster_run_events WHERE run_id=$1`, runID).Scan(&started)
	return
}

// RecordRunEvent 按运行 ID 和递增序号记账，并对规范化后的相同事件去重。
// 诊断摘要随首个有效终态一同保存。已结束运行收到的有效事件只保留审计记录。
func (store *InventoryStore) RecordRunEvent(ctx context.Context, device string, token [32]byte, id string, sequence int64, event workspace.RunEvent) error {
	terminal := event.Kind != "started" && event.Kind != "session"
	if event.SessionFailure != "" && !validSessionFailure(event) {
		return workspace.ErrInvalidSessionFailure
	}
	if len(event.Artifacts) > 0 && (!acceptsArtifacts(event.Kind) || !artifact.ValidFor(id, event.Artifacts)) {
		return workspace.ErrInvalidArtifacts
	}
	if event.InputReceipt != nil && (!terminal || event.Kind == "stopped") {
		return workspace.ErrInvalidInputReceipt
	}
	if event.Diagnostics != nil && (!terminal || !event.Diagnostics.Valid()) {
		return workspace.ErrInvalidRunDiagnostics
	}
	if event.GitVerification != nil && (!terminal || !event.GitVerification.Valid()) {
		return workspace.ErrInvalidGitVerification
	}
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
		if len(event.Artifacts) > 0 && (legacy || run.ManagedGit) {
			return workspace.ErrInvalidArtifacts
		}
		if event.InputReceipt != nil && !run.InputReceiptSupported {
			return workspace.ErrInvalidInputReceipt
		}
		requireCompleteReceipt := event.Kind == "completed" && run.InputReceiptSupported
		if err := validateRunInputReceipt(ctx, tx, id, event.InputReceipt, requireCompleteReceipt); err != nil {
			return err
		}
		if (!run.ManagedGit && event.GitVerification != nil) ||
			(run.ManagedGit && event.Kind == "completed" && (event.GitVerification == nil || event.GitVerification.TestStatus != "passed")) {
			return workspace.ErrInvalidGitVerification
		}
		if event.Diagnostics != nil && len(event.Diagnostics.Reviews) != 0 {
			if !run.ManagedGit {
				return workspace.ErrInvalidRunDiagnostics
			}
			// 有独立 Review 结论的终态必须与同一候选的最后一次审查一致。
			// 旧设备及中断恢复允许仅上报摘要或已持久化的审查记录。
			if summary := event.GitVerification; summary != nil && summary.ReviewStatus != "" {
				last := event.Diagnostics.Reviews[len(event.Diagnostics.Reviews)-1]
				if last.CandidateOID != summary.CandidateOID || last.RepairIndex != summary.RepairAttempts ||
					last.AgentKind != summary.ReviewAgent || last.Status != summary.ReviewStatus {
					return workspace.ErrInvalidRunDiagnostics
				}
			}
		}
		if run.ParentDeviceID != "" {
			event.Text = ""
		}
		event.Lease = ""
		if len(event.WaitForTaskIDs) == 0 {
			event.WaitForTaskIDs = nil
		}
		if len(event.TransferIDs) == 0 {
			event.TransferIDs = nil
		}
		if len(event.Artifacts) == 0 {
			event.Artifacts = nil
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
		outcome := event.Text
		if run.GitPublish && event.Kind == "completed" {
			confirmed, err := requireGitPublished(ctx, tx, id, event.GitVerification)
			if err != nil {
				return err
			}
			if outcome != "" {
				outcome += "\n\n"
			}
			outcome += confirmed
		} else if run.ManagedGit && run.GitReviewSupported && event.Kind == "completed" {
			policy, err := readFrozenGitPolicy(ctx, tx, run.TaskID)
			if err != nil {
				return err
			}
			if !satisfiesGitPolicy(policy, *event.GitVerification) {
				return workspace.ErrInvalidGitVerification
			}
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
		if err := recordRunArtifacts(ctx, tx, run, id, device, sequence, event.Artifacts); err != nil {
			return err
		}
		if run.Status != "queued" && run.Status != "running" {
			// Preserve evidence for a settled round without changing task state or scheduling.
			return advanceWorkspace(ctx, tx)
		}
		if event.SessionFailure != "" {
			return settleSessionFailure(ctx, tx, run, id, sessionID, event)
		}
		// 事件保存原始回执以便去重，结果文本另附已持久化的 Git 回读证据。
		event.Text = outcome
		if event.Kind == "stopped" && (!run.ControlSupported || run.CurrentRun != id || !run.stopping()) {
			return workspace.ErrConflict
		}
		if run.CurrentRun == id {
			if event.Kind == "started" {
				if err := markRunInputsDelivered(ctx, tx, id); err != nil {
					return err
				}
			}
			if terminal {
				if err := requireStartedForInputReceipt(ctx, tx, id, event.InputReceipt); err != nil {
					return err
				}
				if err := markRunInputsHandled(ctx, tx, id, event.InputReceipt); err != nil {
					return err
				}
			}
		}
		if run.CurrentRun == id && run.stopping() && event.Kind != "started" && event.Kind != "session" {
			if run.ControlMode == "cancel" || event.Kind == "stopped" || event.Kind == "interrupted" {
				return settleStoppedRun(ctx, tx, run, id, sessionID, event)
			}
			// 正常退出先于停止指令到达时保留本轮结果，并暂停随后可能出现的续接。
			if _, err := tx.Exec(ctx, "UPDATE muster_tasks SET control_mode='pause' WHERE id=$1", run.TaskID); err != nil {
				return err
			}
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
			if event.Kind == "completed" && run.ParentDeviceID != "" && len(event.TransferIDs) == 0 {
				return workspace.ErrConflict
			}
			if err := recordResultTransfers(ctx, tx, run, id, event.TransferIDs); err != nil {
				return err
			}
		}
		status := event.Kind
		if !terminal {
			status = "running"
		}
		if _, err := tx.Exec(ctx, `UPDATE muster_runs SET status=$2,session_id=$3,outcome=$4,diagnostics=$5,git_verification=$6,updated_at=clock_timestamp()
			WHERE id=$1`, id, status, sessionID, event.Text, event.Diagnostics, event.GitVerification); err != nil {
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
		if run.ParentTaskID != "" && run.ParentDeviceID == "" {
			if err := reconcileCenterTaskGroup(ctx, tx, run.ParentTaskID); err != nil {
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
		var dependency bool
		err := tx.QueryRow(ctx, `SELECT id,ready_priority=$3 AND dependency_results IS NOT NULL FROM muster_runs
			WHERE task_id=$1 AND id<>$2 AND status='queued' ORDER BY ready_priority,sequence LIMIT 1`,
			run.TaskID, runID, priorityDependency).Scan(&next, &dependency)
		if err == nil {
			if dependency {
				reason = ""
			}
			// 依赖续接或已排队的追加要求可能早于上一次的重试轮创建。保持领取代次递增。
			if _, err := tx.Exec(ctx, `UPDATE muster_runs SET generation=(
				SELECT MAX(generation)+1 FROM muster_runs WHERE task_id=$1
			) WHERE id=$2 AND claimed_at IS NULL AND generation<$3`, run.TaskID, next, run.Generation); err != nil {
				return err
			}
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
		control_mode=CASE WHEN $2 IN ('completed','failed','interrupted') THEN '' ELSE control_mode END,
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
			WHERE task_id=$1 AND id<>$2 AND status='queued'`, run.TaskID, runID, run.ParentDeviceID != ""); err != nil {
			return err
		}
	}
	if run.ParentTaskID != "" {
		if run.ParentDeviceID == "" {
			// 中心步骤由运行事件统一汇总，设备子任务则更新父任务的等待记录。
			return nil
		}
		return reconcileParent(ctx, tx, run.TaskID)
	}
	return enqueueTaskReport(ctx, tx, "report-"+runID, run.TaskID, "运行状态", status, outcome)
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
