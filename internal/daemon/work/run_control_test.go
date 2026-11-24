package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

type stopTestHub struct {
	collaborationHub
	check func(control.Identity, string, control.LeaseProof) (bool, error)
}

// RunStopRequested 将设备发起的停止查询交给测试场景判定。
func (hub *stopTestHub) RunStopRequested(_ context.Context, device control.Identity, runID string, proof control.LeaseProof) (bool, error) {
	return hub.check(device, runID, proof)
}

func TestWorkerStopsExactRunBeforeLaunch(t *testing.T) {
	worker, run := claimForStopTest(t, t.TempDir())
	defer worker.Close()
	identity := control.Identity{DeviceID: "device-a"}
	hub := &stopTestHub{check: func(gotDevice control.Identity, gotID string, proof control.LeaseProof) (bool, error) {
		if gotDevice != identity || gotID != run.ID || proof.Generation != run.Generation || proof.Lease != run.Lease {
			t.Fatalf("停止查询未使用当前运行的身份和租约")
		}
		return true, nil
	}}
	peer := &collaboration{client: hub, identity: identity}
	invoke := func(context.Context, string, string, string, string, []string, func() error, func(string) error) (string, error) {
		t.Fatal("设备已在启动前收到停止指令")
		return "", nil
	}
	if err := worker.execute(t.Context(), run, peer, nil, invoke); err != nil {
		t.Fatal(err)
	}
	assertStopReceipt(t, worker, run)
}

func TestWorkerPollsForStopDuringRunAndKeepsSession(t *testing.T) {
	worker, run := claimForStopTest(t, t.TempDir())
	defer worker.Close()
	var checks atomic.Int32
	hub := &stopTestHub{check: func(_ control.Identity, _ string, _ control.LeaseProof) (bool, error) {
		if checks.Add(1) == 1 {
			return false, errors.New("临时断线")
		}
		return true, nil
	}}
	peer := &collaboration{client: hub}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	invoke := func(ctx context.Context, _, _, _, _ string, _ []string, _ func() error, session func(string) error) (string, error) {
		if err := session(run.SessionID); err != nil {
			return "", err
		}
		<-ctx.Done()
		if !errors.Is(context.Cause(ctx), errRunStopRequested) {
			t.Errorf("运行收到非停止原因：%v", context.Cause(ctx))
		}
		return "部分输出", ctx.Err()
	}
	if err := worker.execute(ctx, run, peer, nil, invoke); err != nil {
		t.Fatal(err)
	}
	assertStopReceipt(t, worker, run)
}

func TestWorkerWithholdsStopReceiptIfProcessExitIsUnconfirmed(t *testing.T) {
	profile := t.TempDir()
	worker, run := claimForStopTest(t, profile)
	defer func() {
		if worker != nil {
			_ = worker.Close()
		}
	}()
	var checks atomic.Int32
	hub := &stopTestHub{check: func(_ control.Identity, _ string, _ control.LeaseProof) (bool, error) {
		return checks.Add(1) > 1, nil
	}}
	peer := &collaboration{client: hub}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var exitProofPath string
	invoke := func(ctx context.Context, _, _, _, _ string, _ []string, _ func() error, _ func(string) error) (string, error) {
		if worker.processProofs != nil {
			pending, arm, err := worker.reserveProcessLaunch(ctx, run.ID, run.Generation, false)
			if err != nil {
				return "", err
			}
			if err := arm(); err != nil {
				return "", err
			}
			exitProofPath = pending
		}
		<-ctx.Done()
		return "", runner.ErrProcessExitUnconfirmed
	}
	if err := worker.execute(ctx, run, peer, nil, invoke); !errors.Is(err, runner.ErrProcessExitUnconfirmed) {
		t.Fatalf("无法确认进程退出时应保留恢复记录，收到 %v", err)
	}
	var state string
	var receipts int
	if err := worker.db.QueryRowContext(ctx, `SELECT state,(SELECT count(*) FROM outbox) FROM runs WHERE id=?`, run.ID).Scan(&state, &receipts); err != nil {
		t.Fatal(err)
	}
	if state != "starting" || receipts != 0 {
		t.Fatalf("退出尚未确认时得到状态 %q，回执数 %d", state, receipts)
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	worker, err = Open(profile)
	if err != nil {
		t.Fatal(err)
	}
	if exitProofPath != "" {
		if err := worker.recoverInterrupted(t.Context()); !errors.Is(err, runner.ErrProcessExitUnconfirmed) {
			t.Fatalf("等待守护进程确认退出时不应恢复终态：%v", err)
		}
		marker := "muster-process-exit-v1\n" + filepath.Base(exitProofPath) + "\n"
		if err := os.WriteFile(exitProofPath, []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := worker.recoverInterrupted(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var payload []byte
	if err := worker.db.QueryRowContext(t.Context(), `SELECT count(*),max(payload) FROM outbox WHERE run_id=?`, run.ID).Scan(&receipts, &payload); err != nil {
		t.Fatal(err)
	}
	var saved control.RunEvent
	if err := json.Unmarshal(payload, &saved); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || saved.Kind != "interrupted" || saved.Lease != run.Lease || saved.Generation != run.Generation || saved.SessionID != run.SessionID {
		t.Fatalf("设备重启必须保留当前代次的唯一中断回执，收到 %d 条，事件 %s", receipts, saved.Kind)
	}
}

func claimForStopTest(t *testing.T, profile string) (*Worker, control.Run) {
	t.Helper()
	worker, err := Open(profile)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := worker.RegisterDirectory(t.Context(), t.TempDir())
	if err != nil {
		_ = worker.Close()
		t.Fatal(err)
	}
	run := control.Run{ID: "run-current", TaskID: "task-current", DirectoryID: directory.ID, Prompt: "fixture",
		SessionID: "native-session", Lease: "fixture-lease", Generation: 3, ControlSupported: true,
		LeaseUntil: time.Now().Add(time.Minute).Format(time.RFC3339Nano)}
	if err := worker.saveClaim(t.Context(), run); err != nil {
		_ = worker.Close()
		t.Fatal(err)
	}
	return worker, run
}

func assertStopReceipt(t *testing.T, worker *Worker, run control.Run) {
	t.Helper()
	var state string
	var payload []byte
	if err := worker.db.QueryRowContext(t.Context(), `SELECT r.state,o.payload FROM runs r
		JOIN outbox o ON o.run_id=r.id AND o.sequence=r.sequence WHERE r.id=?`, run.ID).Scan(&state, &payload); err != nil {
		t.Fatal(err)
	}
	var saved control.RunEvent
	if err := json.Unmarshal(payload, &saved); err != nil {
		t.Fatal(err)
	}
	if state != "interrupted" || saved.Kind != "stopped" || saved.Lease != run.Lease || saved.Generation != run.Generation || saved.SessionID != run.SessionID {
		t.Fatalf("本地停止状态 %q，停止回执 %v", state, saved.Kind)
	}
}
