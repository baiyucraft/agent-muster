package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"

	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

var (
	publicationOID         = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
	destinationFingerprint = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// AuthorizeGitPush 为候选冻结一次推送意图。重复请求只能取得相同意图，实际推送还须满足当前任务状态。
func (store *InventoryStore) AuthorizeGitPush(ctx context.Context, device string, token [32]byte, id string, input gitdelivery.PushRequest) (value gitdelivery.PushIntent, err error) {
	if !input.Verification.Valid() || input.Verification.TestStatus != "passed" || !destinationFingerprint.MatchString(input.DestinationFingerprint) {
		return value, workspace.ErrInvalidGitPublication
	}
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		run, err := readRunState(ctx, tx, device, id)
		if err != nil {
			return err
		}
		if !run.GitPublish || !run.ControlSupported || input.Lease == "" || run.Lease != input.Lease || run.Generation != input.Generation {
			return workspace.ErrConflict
		}
		intent, evidence, err := readGitPush(ctx, tx, id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if intent.ID != "" && (!reflect.DeepEqual(evidence, input.Verification) || intent.DestinationFingerprint != input.DestinationFingerprint) {
			return workspace.ErrConflict
		}
		if intent.ID != "" && intent.Status != "pending" {
			value = intent
			return nil
		}
		allowed, err := gitPushAllowed(ctx, tx, run, id)
		if err != nil {
			return err
		}
		if intent.ID != "" {
			intent.PushAllowed = allowed
			value = intent
			return nil
		}
		if !allowed {
			return workspace.ErrConflict
		}
		policy, err := readFrozenGitPolicy(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		if !satisfiesGitPolicy(policy, input.Verification) {
			return workspace.ErrInvalidGitPublication
		}
		target := policy.TargetBranch
		if target == "" {
			target = "refs/heads/muster/" + run.TaskID
		}
		value = gitdelivery.PushIntent{Publication: gitdelivery.Publication{
			ID: "push-" + stableUUID(id), Revision: policy.Revision, Remote: policy.Remote, TargetRef: target,
			CandidateOID: input.Verification.CandidateOID, Status: "pending",
		}, DestinationFingerprint: input.DestinationFingerprint, PushAllowed: true}
		if _, err := tx.Exec(ctx, `INSERT INTO muster_git_push_intents
			(id,run_id,task_id,policy_revision,remote,target_ref,destination_fingerprint,verification)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, value.ID, id, run.TaskID, value.Revision, value.Remote,
			value.TargetRef, value.DestinationFingerprint, input.Verification); err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
	return value, err
}

// ObserveGitPush 保存既有意图的远端回读。租约到期后仍可补报，且回读不能颁发推送许可。
func (store *InventoryStore) ObserveGitPush(ctx context.Context, device string, token [32]byte, id, intentID string, input gitdelivery.Observation) (value gitdelivery.PushIntent, err error) {
	err = store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if err := authenticateDevice(ctx, tx, device, token); err != nil {
			return err
		}
		run, err := readRunState(ctx, tx, device, id)
		if err != nil {
			return err
		}
		if input.Lease == "" || run.Lease != input.Lease || run.Generation != input.Generation {
			return workspace.ErrConflict
		}
		intent, _, err := readGitPush(ctx, tx, id)
		if err != nil {
			return notFound(err)
		}
		if intent.ID != intentID {
			return workspace.ErrConflict
		}
		if !validGitObservation(input, intent.CandidateOID) {
			return workspace.ErrInvalidGitPublication
		}
		value = intent
		if gitPublished(intent.Status) || intent.Status == input.Status && intent.ObservedOID == input.ObservedOID {
			return nil
		}
		value.Status, value.ObservedOID = input.Status, input.ObservedOID
		if _, err := tx.Exec(ctx, `UPDATE muster_git_push_intents SET status=$2,observed_oid=$3,updated_at=clock_timestamp()
			WHERE id=$1`, intent.ID, value.Status, value.ObservedOID); err != nil {
			return err
		}
		return advanceWorkspace(ctx, tx)
	})
	return value, err
}

func gitPushAllowed(ctx context.Context, tx pgx.Tx, run runState, id string) (bool, error) {
	if !run.LeaseLive || run.CurrentRun != id || run.Status != "running" || run.ControlMode != "" || run.WaitingReason != "" {
		return false, nil
	}
	var blocked bool
	err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM muster_messages WHERE requested_task_id=$1 AND status='accepted') OR
		EXISTS (SELECT 1 FROM muster_runs WHERE task_id=$1 AND id<>$2 AND status='queued') OR
		EXISTS (SELECT 1 FROM muster_git_push_intents WHERE task_id=$1 AND run_id<>$2 AND status IN ('pending','unavailable'))`,
		run.TaskID, id).Scan(&blocked)
	return !blocked, err
}

func readFrozenGitPolicy(ctx context.Context, tx pgx.Tx, task string) (policy gitdelivery.Policy, err error) {
	err = tx.QueryRow(ctx, "SELECT git_policy FROM muster_tasks WHERE id=$1", task).Scan(&policy)
	return
}

func satisfiesGitPolicy(policy gitdelivery.Policy, evidence gitdelivery.Verification) bool {
	if evidence.RepairAttempts > policy.MaxRepairs || evidence.ReviewAgent != policy.ReviewAgent {
		return false
	}
	return policy.ReviewAgent == "" || evidence.ReviewStatus == "passed"
}

func readGitPush(ctx context.Context, tx pgx.Tx, runID string) (value gitdelivery.PushIntent, evidence gitdelivery.Verification, err error) {
	err = tx.QueryRow(ctx, `SELECT id,policy_revision,remote,target_ref,verification,destination_fingerprint,status,observed_oid
		FROM muster_git_push_intents WHERE run_id=$1`, runID).
		Scan(&value.ID, &value.Revision, &value.Remote, &value.TargetRef, &evidence, &value.DestinationFingerprint, &value.Status, &value.ObservedOID)
	value.CandidateOID = evidence.CandidateOID
	return
}

func requireGitPublished(ctx context.Context, tx pgx.Tx, runID string, evidence *gitdelivery.Verification) (string, error) {
	intent, frozen, err := readGitPush(ctx, tx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", workspace.ErrConflict
	}
	if err != nil {
		return "", err
	}
	if evidence == nil || !gitPublished(intent.Status) || !reflect.DeepEqual(frozen, *evidence) {
		return "", workspace.ErrConflict
	}
	result := "Git 测试通过（退出码 0）"
	if frozen.ReviewAgent != "" {
		result += "\n独立 Review 通过（" + frozen.ReviewAgent + "）"
	}
	result += fmt.Sprintf("\n候选提交 %s\n目标分支 %s · %s\n远端分支头 %s",
		intent.CandidateOID, intent.Remote, intent.TargetRef, intent.ObservedOID)
	return result, nil
}

func gitPublished(status string) bool {
	return status == "same_commit" || status == "contains_commit"
}

func validGitObservation(observation gitdelivery.Observation, candidate string) bool {
	switch observation.Status {
	case "same_commit":
		return observation.ObservedOID == candidate
	case "contains_commit", "diverged":
		return observation.ObservedOID != candidate && len(observation.ObservedOID) == len(candidate) && publicationOID.MatchString(observation.ObservedOID)
	case "missing", "unavailable":
		return observation.ObservedOID == ""
	default:
		return false
	}
}
