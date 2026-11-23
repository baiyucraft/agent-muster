package work

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/gitwork"
	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
)

type gitRecoveryHub struct {
	collaborationHub
	intent      gitdelivery.PushIntent
	observation gitdelivery.Observation
	stop        bool
}

// AuthorizeGitPush 检测恢复流程是否尝试重新取得已保存意图的推送许可。
func (*gitRecoveryHub) AuthorizeGitPush(context.Context, control.Identity, string, gitdelivery.PushRequest) (gitdelivery.PushIntent, error) {
	return gitdelivery.PushIntent{}, fmt.Errorf("恢复期间请求推送授权")
}

// ObserveGitPush 记录恢复过程中提交的远端状态。
func (hub *gitRecoveryHub) ObserveGitPush(_ context.Context, _ control.Identity, _, _ string, input gitdelivery.Observation) (gitdelivery.PushIntent, error) {
	hub.observation = input
	result := hub.intent
	result.Status, result.ObservedOID = input.Status, input.ObservedOID
	return result, nil
}

// RunStopRequested 返回测试设置的停止请求。
func (hub *gitRecoveryHub) RunStopRequested(context.Context, control.Identity, string, control.LeaseProof) (bool, error) {
	return hub.stop, nil
}

func TestGitPushRecoveryDoesNotRepeatUncertainRemoteWrite(t *testing.T) {
	if _, err := exec.LookPath("git.exe"); err != nil {
		t.Skip("Git is required")
	}
	for _, kind := range []string{"completed", "stopped", "interrupted"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			profile := t.TempDir()
			worker, run := claimForStopTest(t, profile)
			defer func() { _ = worker.Close() }()
			root, err := worker.resolveDirectory(ctx, run.DirectoryID)
			if err != nil {
				t.Fatal(err)
			}
			git := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Dir = dir
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("Git %v: %v\n%s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			remote := t.TempDir()
			git(remote, "init", "--bare")
			git(root, "init", "--initial-branch=main")
			git(root, "-c", "user.name=Muster fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "candidate")
			git(root, "remote", "add", "origin", remote)
			candidate := git(root, "rev-parse", "HEAD")
			destination, err := gitwork.ResolvePushDestination(ctx, root, "origin")
			if err != nil {
				t.Fatal(err)
			}
			intent := gitdelivery.PushIntent{Publication: gitdelivery.Publication{
				ID: "push-fixture", Remote: "origin", TargetRef: "refs/heads/muster/fixture", CandidateOID: candidate,
			}, DestinationFingerprint: destination.Fingerprint()}
			event := control.RunEvent{Kind: "completed", Text: "候选验证已完成", Generation: run.Generation, Lease: run.Lease, SessionID: run.SessionID}
			attempt := gitPushAttempt{Intent: intent, Result: event, WriteUncertain: true,
				Request: gitdelivery.PushRequest{Generation: run.Generation, Lease: run.Lease}}
			if kind == "interrupted" {
				attempt.RecheckUntil = time.Now().Add(-time.Second)
			}
			if err := worker.markLaunching(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			if err := worker.saveGitAttempt(ctx, run.ID, attempt); err != nil {
				t.Fatal(err)
			}
			if err := worker.Close(); err != nil {
				t.Fatal(err)
			}
			worker, err = Open(profile)
			if err != nil {
				t.Fatal(err)
			}
			if err := worker.recoverInterrupted(ctx); err != nil {
				t.Fatal(err)
			}
			hub := &gitRecoveryHub{intent: intent, stop: kind == "stopped"}
			peer := &collaboration{client: hub}
			if active, err := worker.recoverGitPush(ctx, peer); !active || err != nil {
				t.Fatalf("首次恢复 active=%t，err=%v", active, err)
			}
			if hub.observation.Status != "unavailable" || git(remote, "for-each-ref", intent.TargetRef) != "" {
				t.Fatalf("未确认的写入不应被重推或判为失败：%+v", hub.observation)
			}
			if active, err := worker.recoverGitPush(ctx, peer); err != nil || active != (kind == "completed") {
				t.Fatalf("停止或等待超时后需释放执行名额，active=%t，err=%v", active, err)
			}
			// 模拟第一次推送稍后在远端完成。恢复仅核对这个结果。
			git(root, "push", "origin", "HEAD:"+intent.TargetRef)
			if _, err := worker.db.ExecContext(ctx, `UPDATE git_push_attempts SET next_check=0 WHERE run_id=?`, run.ID); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := worker.recoverGitPush(ctx, peer); err != nil {
					t.Fatal(err)
				}
			}
			var receipts, pending int
			var payload []byte
			if err := worker.db.QueryRowContext(ctx, `SELECT count(*),max(payload),(SELECT count(*) FROM git_push_attempts) FROM outbox WHERE run_id=?`, run.ID).Scan(&receipts, &payload, &pending); err != nil {
				t.Fatal(err)
			}
			var result control.RunEvent
			if err := json.Unmarshal(payload, &result); err != nil {
				t.Fatal(err)
			}
			if receipts != 1 || pending != 0 || result.Kind != kind || hub.observation.ObservedOID != candidate {
				t.Fatalf("恢复未保留唯一回执：%d，待核对 %d，事件 %s，远端 %+v", receipts, pending, result.Kind, hub.observation)
			}
		})
	}
}
