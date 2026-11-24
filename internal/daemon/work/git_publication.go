package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/gitwork"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
)

var errGitPushPending = errors.New("等待核对 Git 远端分支")
var errGitPushForbidden = errors.New("Hub 已撤回本轮 Git 推送许可")

const gitConfirmationWindow = time.Minute

type gitPushClient interface {
	AuthorizeGitPush(context.Context, control.Identity, string, gitdelivery.PushRequest) (gitdelivery.PushIntent, error)
	ObserveGitPush(context.Context, control.Identity, string, string, gitdelivery.Observation) (gitdelivery.PushIntent, error)
}

type gitPushAttempt struct {
	Request        gitdelivery.PushRequest `json:"request"`
	Intent         gitdelivery.PushIntent  `json:"intent"`
	Result         control.RunEvent        `json:"result"`
	WriteUncertain bool                    `json:"write_uncertain,omitempty"`
	RecheckUntil   time.Time               `json:"recheck_until"`
}

func peerGitClient(peer *collaboration) (gitPushClient, bool) {
	if peer == nil {
		return nil, false
	}
	client, ok := peer.client.(gitPushClient)
	return client, ok
}

func (worker *Worker) freezeGitDestination(ctx context.Context, run control.Run, dir string) (string, error) {
	destination, err := gitwork.ResolvePushDestination(ctx, dir, run.GitPolicy.Remote)
	if err != nil {
		return "", err
	}
	fingerprint := destination.Fingerprint()
	if _, err := worker.db.ExecContext(ctx, `INSERT INTO git_destinations(task_id,fingerprint) VALUES(?,?) ON CONFLICT(task_id) DO NOTHING`, run.TaskID, fingerprint); err != nil {
		return "", err
	}
	var previous string
	if err := worker.db.QueryRowContext(ctx, `SELECT fingerprint FROM git_destinations WHERE task_id=?`, run.TaskID).Scan(&previous); err != nil {
		return "", err
	}
	if fingerprint != previous {
		return "", errors.New("Git 推送地址与首次执行时的配置不一致")
	}
	return fingerprint, nil
}

func (worker *Worker) publishGitCandidate(ctx context.Context, run control.Run, peer *collaboration, dir string, checkout gitwork.Workspace, fingerprint string, event control.RunEvent) error {
	current, err := checkout.Candidate(ctx)
	if err != nil {
		return err
	}
	if current != event.GitVerification.CandidateOID {
		return errors.New("Git 候选在交付前改变")
	}
	client, _ := peerGitClient(peer)
	if _, err := resolveGitDestination(ctx, dir, run.GitPolicy.Remote, fingerprint); err != nil {
		return err
	}
	attempt := gitPushAttempt{Request: gitdelivery.PushRequest{
		Generation: run.Generation, Lease: run.Lease, Verification: *event.GitVerification, DestinationFingerprint: fingerprint,
		InputReceipt: event.InputReceipt,
	}, Result: event, RecheckUntil: time.Now().Add(gitConfirmationWindow)}
	// 先记录申请及候选。Hub 已提交但响应丢失时，重启可用原请求找回意图。
	if err := worker.saveGitAttempt(ctx, run.ID, attempt); err != nil {
		return err
	}
	intent, err := client.AuthorizeGitPush(ctx, peer.identity, run.ID, attempt.Request)
	if gitAuthorizationRejected(err) {
		if clearErr := worker.clearGitAttempt(context.WithoutCancel(ctx), run.ID); clearErr != nil {
			return errors.Join(errGitPushPending, clearErr)
		}
		return err
	}
	if err != nil {
		return errors.Join(errGitPushPending, err)
	}
	if err := matchGitIntent(run, attempt.Request, intent); err != nil {
		return errors.Join(errGitPushPending, err)
	}
	attempt.Intent = intent
	if err := worker.saveGitAttempt(ctx, run.ID, attempt); err != nil {
		return errors.Join(errGitPushPending, err)
	}
	observation, err := readGitBranch(ctx, dir, attempt)
	if err != nil {
		return err
	}
	if intent.PushAllowed && (observation.Status == "missing" || observation.Status == "diverged") {
		destination, err := resolveGitDestination(ctx, dir, intent.Remote, fingerprint)
		if err != nil {
			if errors.Is(err, runner.ErrProcessExitUnconfirmed) {
				return err
			}
			return errors.Join(errGitPushPending, err)
		}
		pushErr := gitwork.PushCandidate(ctx, dir, destination, intent.TargetRef, intent.CandidateOID, func() error {
			// 远端读回和本地准备完成后，再对当前输入、停止指令和租约进行核准。
			latest, err := client.AuthorizeGitPush(ctx, peer.identity, run.ID, attempt.Request)
			if err != nil {
				return errors.Join(errGitPushPending, err)
			}
			if !latest.PushAllowed || latest.Publication != intent.Publication || latest.DestinationFingerprint != intent.DestinationFingerprint {
				return errGitPushForbidden
			}
			pending := attempt
			pending.WriteUncertain = true
			pending.RecheckUntil = time.Now().Add(gitConfirmationWindow)
			if err := worker.saveGitAttempt(ctx, run.ID, pending); err != nil {
				return errors.Join(errGitPushPending, err)
			}
			attempt = pending
			return nil
		})
		if errors.Is(pushErr, runner.ErrProcessExitUnconfirmed) {
			return pushErr
		}
		if errors.Is(pushErr, errGitPushPending) {
			return pushErr
		}
		attempt.RecheckUntil = time.Now().Add(gitConfirmationWindow)
		if pushErr == nil || errors.Is(pushErr, gitwork.ErrPushRejected) {
			attempt.WriteUncertain = false
			if err := worker.saveGitAttempt(context.WithoutCancel(ctx), run.ID, attempt); err != nil {
				return errors.Join(errGitPushPending, err)
			}
		}
		observation, err = readGitBranch(ctx, dir, attempt)
		if err != nil {
			return err
		}
	}
	return worker.confirmGitAttempt(ctx, peer, run.ID, attempt, observation)
}

func matchGitIntent(run control.Run, request gitdelivery.PushRequest, intent gitdelivery.PushIntent) error {
	target := run.GitPolicy.TargetBranch
	if target == "" {
		target = "refs/heads/muster/" + run.TaskID
	}
	if intent.Revision != run.GitPolicy.Revision || intent.TargetRef != target || intent.Remote != run.GitPolicy.Remote ||
		intent.CandidateOID != request.Verification.CandidateOID || intent.DestinationFingerprint != request.DestinationFingerprint {
		return errors.New("Hub 的 Git 推送意图与冻结的项目设置不一致")
	}
	return nil
}

func resolveGitDestination(ctx context.Context, dir, remote, fingerprint string) (gitwork.PushDestination, error) {
	destination, err := gitwork.ResolvePushDestination(ctx, dir, remote)
	if err == nil && destination.Fingerprint() != fingerprint {
		err = errors.New("Git 推送地址已改变，等待恢复原远端配置")
	}
	return destination, err
}

func readGitBranch(ctx context.Context, dir string, attempt gitPushAttempt) (gitdelivery.Observation, error) {
	intent := attempt.Intent
	result := gitdelivery.Observation{Generation: attempt.Request.Generation, Lease: attempt.Request.Lease, Status: "unavailable"}
	if dir == "" {
		return result, nil
	}
	destination, err := resolveGitDestination(ctx, dir, intent.Remote, intent.DestinationFingerprint)
	if err != nil {
		if errors.Is(err, runner.ErrProcessExitUnconfirmed) {
			return result, err
		}
		return result, nil
	}
	head, contains, err := gitwork.ConfirmCandidate(ctx, dir, destination, intent.TargetRef, intent.CandidateOID)
	if err != nil {
		if errors.Is(err, runner.ErrProcessExitUnconfirmed) {
			return result, err
		}
		return result, nil
	}
	result.ObservedOID = head
	switch {
	case head == "":
		result.Status = "missing"
	case head == intent.CandidateOID:
		result.Status = "same_commit"
	case contains:
		result.Status = "contains_commit"
	default:
		result.Status = "diverged"
	}
	return result, nil
}

func (worker *Worker) confirmGitAttempt(ctx context.Context, peer *collaboration, runID string, attempt gitPushAttempt, observation gitdelivery.Observation) error {
	client, _ := peerGitClient(peer)
	// 推送响应丢失时，一次未找到候选的读回无法证明远端已结束处理。
	if attempt.WriteUncertain && (observation.Status == "missing" || observation.Status == "diverged") {
		observation.Status, observation.ObservedOID = "unavailable", ""
	}
	confirmed, err := client.ObserveGitPush(ctx, peer.identity, runID, attempt.Intent.ID, observation)
	if err != nil {
		return errors.Join(errGitPushPending, err)
	}
	switch confirmed.Status {
	case "same_commit", "contains_commit":
		return nil
	case "missing", "diverged":
		return errors.New("Git 远端分支尚未包含候选提交 " + attempt.Intent.CandidateOID)
	default:
		return errGitPushPending
	}
}

func (worker *Worker) saveGitAttempt(ctx context.Context, runID string, attempt gitPushAttempt) error {
	data, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	_, err = worker.db.ExecContext(ctx, `INSERT INTO git_push_attempts(run_id,data) VALUES(?,?) ON CONFLICT(run_id) DO UPDATE SET data=excluded.data`, runID, data)
	return err
}

func (worker *Worker) clearGitAttempt(ctx context.Context, runID string) error {
	tx, err := worker.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM git_push_attempts WHERE run_id=?`, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM process_runs WHERE run_id=?
		AND EXISTS (SELECT 1 FROM runs WHERE id=? AND state NOT IN ('starting','started'))`, runID, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func gitAuthorizationRejected(err error) bool {
	var status *control.HTTPError
	return errors.As(err, &status) && (status.Status == http.StatusConflict || status.Status == http.StatusBadRequest ||
		status.Status == http.StatusUnprocessableEntity || status.Status == http.StatusRequestEntityTooLarge)
}

func (worker *Worker) recoverGitPush(ctx context.Context, peer *collaboration) (bool, error) {
	if err := worker.checkProcessRecovery(ctx); err != nil {
		return false, err
	}
	// 只恢复读回。上一进程可能已写入远端，即使本机未收到 Git 或 Hub 的响应。
	var runID, state string
	var data, claim []byte
	var active bool
	err := worker.db.QueryRowContext(ctx, `SELECT p.run_id,r.state,p.data,c.data FROM git_push_attempts p
		JOIN runs r ON r.id=p.run_id JOIN run_claims c ON c.id=p.run_id
		WHERE p.next_check<=? ORDER BY (r.state IN ('starting','started')) DESC,p.next_check,p.rowid LIMIT 1`,
		time.Now().UnixMilli()).Scan(&runID, &state, &data, &claim)
	if errors.Is(err, sql.ErrNoRows) {
		err = worker.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM git_push_attempts p
			JOIN runs r ON r.id=p.run_id WHERE r.state IN ('starting','started'))`).Scan(&active)
		return active, err
	}
	if err != nil {
		return false, err
	}
	err = worker.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM git_push_attempts p
		JOIN runs r ON r.id=p.run_id WHERE r.state IN ('starting','started'))`).Scan(&active)
	if err != nil {
		return false, err
	}
	err = worker.reconcileGitAttempt(ctx, peer, runID, state, data, claim)
	if !errors.Is(err, errGitPushPending) || errors.Is(err, control.ErrUnauthorized) {
		return active, err
	}
	// 已停止的待核对记录仍会重试，允许设备领取无关工作。
	_, err = worker.db.ExecContext(ctx, `UPDATE git_push_attempts SET next_check=? WHERE run_id=?`, time.Now().Add(5*time.Second).UnixMilli(), runID)
	return active, err
}

func (worker *Worker) reconcileGitAttempt(ctx context.Context, peer *collaboration, runID, state string, data, claim []byte) error {
	client, ok := peerGitClient(peer)
	if !ok {
		return errGitPushPending
	}
	var run control.Run
	var attempt gitPushAttempt
	if err := errors.Join(json.Unmarshal(data, &attempt), json.Unmarshal(claim, &run)); err != nil {
		return err
	}
	if attempt.Intent.ID == "" {
		intent, err := client.AuthorizeGitPush(ctx, peer.identity, runID, attempt.Request)
		if gitAuthorizationRejected(err) {
			return worker.finishRecoveredGitPush(ctx, runID, state, attempt.Result, err)
		}
		if err != nil {
			return errors.Join(errGitPushPending, err)
		}
		if err := matchGitIntent(run, attempt.Request, intent); err != nil {
			return errors.Join(errGitPushPending, err)
		}
		attempt.Intent = intent
		if err := worker.saveGitAttempt(ctx, runID, attempt); err != nil {
			return err
		}
	}
	check, cancel := context.WithTimeout(ctx, 15*time.Second)
	check = worker.withProcessRecovery(check, runID, run.Generation, true)
	dir, _ := worker.resolveDirectory(check, run.DirectoryID)
	observation, err := readGitBranch(check, dir, attempt)
	cancel()
	if err != nil {
		return err
	}
	if err := worker.checkProcessLaunches(ctx); err != nil {
		return err
	}
	err = worker.confirmGitAttempt(ctx, peer, runID, attempt, observation)
	if errors.Is(err, errGitPushPending) {
		if state == "starting" || state == "started" {
			proof := control.LeaseProof{Generation: run.Generation, Lease: run.Lease}
			if stop, _ := peer.client.RunStopRequested(ctx, peer.identity, runID, proof); stop {
				attempt.Result.Kind, attempt.Result.Text = "stopped", "设备已停止本轮执行，Git 远端分支仍待核对。"
				return worker.stageEvent(ctx, runID, attempt.Result)
			}
			if !attempt.RecheckUntil.IsZero() && !time.Now().Before(attempt.RecheckUntil) {
				attempt.Result.Kind, attempt.Result.Text = "interrupted", "本轮执行已结束，Git 远端分支仍待核对。设备将继续读回。"
				return worker.stageEvent(ctx, runID, attempt.Result)
			}
		}
		return err
	}
	return worker.finishRecoveredGitPush(ctx, runID, state, attempt.Result, err)
}

func (worker *Worker) finishRecoveredGitPush(ctx context.Context, runID, state string, event control.RunEvent, err error) error {
	if state == "starting" || state == "started" {
		if err != nil {
			event.Kind = "failed"
			event.Text = err.Error() + "\n\n" + event.Text
		}
		event.Text = trimUTF8(event.Text, 12000)
		return worker.stageEventWithResult(ctx, runID, event, control.Manifest{}, nil, true)
	}
	return worker.clearGitAttempt(ctx, runID)
}
