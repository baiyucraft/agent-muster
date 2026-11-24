package work

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

func TestProcessRecoveryKeepsRunUntilExactProofArrives(t *testing.T) {
	profile := t.TempDir()
	worker, run := claimForStopTest(t, profile)
	if err := worker.markLaunching(t.Context(), run.ID); err != nil {
		worker.Close()
		t.Fatal(err)
	}
	path, arm, err := worker.reserveProcessLaunch(t.Context(), run.ID, run.Generation, false)
	if err != nil {
		worker.Close()
		t.Fatal(err)
	}
	if err := arm(); err != nil {
		worker.Close()
		t.Fatal(err)
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	worker, err = Open(profile)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()

	// 文件存在但内容归属另一轮启动时，仍不得恢复或领取。
	wrongProof := "muster-process-exit-v1\nexit-00000000000000000000000000000000.proof\n"
	if err := os.WriteFile(path, []byte(wrongProof), 0600); err != nil {
		t.Fatal(err)
	}
	hub := &fakeHub{}
	errs := make(chan error, 8)
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	go func() {
		done <- worker.RunWithExecutor(ctx, hub, control.Identity{}, func(err error) {
			select {
			case errs <- err:
			default:
			}
		}, func(context.Context, string, string, string, func() error, func(string) error) (string, error) {
			t.Error("恢复旧轮时启动了新执行")
			return "", errors.New("unexpected execution")
		})
	}()
	select {
	case err := <-errs:
		if !errors.Is(err, runner.ErrProcessExitUnconfirmed) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("未报告待核对进程树")
	}
	hub.mu.Lock()
	claimsBeforeProof, reportsBeforeProof := hub.claimAttempts, len(hub.receipts)
	// 让假 Hub 维持空队列，恢复后仍可观测领取请求。
	hub.claimed = true
	hub.mu.Unlock()
	if claimsBeforeProof != 0 || reportsBeforeProof != 0 {
		t.Fatalf("旧轮尚在等待退出证据，收到 %d 次领取和 %d 条回执", claimsBeforeProof, reportsBeforeProof)
	}
	proof := "muster-process-exit-v1\n" + filepath.Base(path) + "\n"
	if err := os.WriteFile(path, []byte(proof), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		hub.mu.Lock()
		hasClaim := hub.claimAttempts != 0
		receiptCount := len(hub.receipts)
		hub.mu.Unlock()
		if hasClaim {
			if receiptCount != 1 {
				t.Fatalf("恢复后应只上报一条中断，收到 %d 条", receiptCount)
			}
			break
		}
		if !sleepContext(ctx, 10*time.Millisecond) {
			t.Fatal("退出凭据有效后未恢复领取")
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("已结清的凭据应删除：%v", err)
	}
}

func TestProcessRecoveryRejectsLegacyOrMismatchedGeneration(t *testing.T) {
	for _, state := range []string{"legacy", "legacy Git readback", "different generation", "never armed"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			worker, run := claimForStopTest(t, t.TempDir())
			defer worker.Close()
			switch state {
			case "legacy":
				if _, err := worker.db.ExecContext(ctx, `UPDATE runs SET state='starting' WHERE id=?`, run.ID); err != nil {
					t.Fatal(err)
				}
			case "legacy Git readback":
				if _, err := worker.db.ExecContext(ctx, `UPDATE runs SET state='interrupted' WHERE id=?`, run.ID); err != nil {
					t.Fatal(err)
				}
				if err := worker.saveGitAttempt(ctx, run.ID, gitPushAttempt{}); err != nil {
					t.Fatal(err)
				}
			default:
				if err := worker.markLaunching(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
				path, arm, err := worker.reserveProcessLaunch(ctx, run.ID, run.Generation, false)
				if err != nil {
					t.Fatal(err)
				}
				if state == "different generation" {
					if err := arm(); err != nil {
						t.Fatal(err)
					}
					if _, err := worker.db.ExecContext(ctx, `UPDATE process_launches SET generation=generation+1`); err != nil {
						t.Fatal(err)
					}
					proof := "muster-process-exit-v1\n" + filepath.Base(path) + "\n"
					if err := os.WriteFile(path, []byte(proof), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if state == "never armed" {
				if err := worker.recoverInterrupted(ctx); err != nil {
					t.Fatal(err)
				}
				var launches int
				if err := worker.db.QueryRowContext(ctx, `SELECT count(*) FROM process_launches`).Scan(&launches); err != nil || launches != 0 {
					t.Fatalf("未取得执行许可的记录应清理：%d，%v", launches, err)
				}
				return
			}
			if err := worker.recoverInterrupted(ctx); !errors.Is(err, runner.ErrProcessExitUnconfirmed) {
				t.Fatalf("恢复未保留证据不明的活动运行：%v", err)
			}
			if _, err := worker.recoverGitPush(ctx, nil); !errors.Is(err, runner.ErrProcessExitUnconfirmed) {
				t.Fatalf("Git 恢复绕过进程门禁：%v", err)
			}
		})
	}
}

func TestGitReadbackProcessExitRemainsRequiredAfterRunTerminal(t *testing.T) {
	ctx := t.Context()
	worker, run := claimForStopTest(t, t.TempDir())
	defer worker.Close()
	if err := worker.markLaunching(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := worker.saveGitAttempt(ctx, run.ID, gitPushAttempt{}); err != nil {
		t.Fatal(err)
	}
	if err := worker.stageEvent(ctx, run.ID, control.RunEvent{Kind: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	path, arm, err := worker.reserveProcessLaunch(ctx, run.ID, run.Generation, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := arm(); err != nil {
		t.Fatal(err)
	}
	if err := worker.recoverInterrupted(ctx); !errors.Is(err, runner.ErrProcessExitUnconfirmed) {
		t.Fatalf("旧轮已提交终态，仍需核对其异步 Git 读回的进程树：%v", err)
	}
	proof := "muster-process-exit-v1\n" + filepath.Base(path) + "\n"
	if err := os.WriteFile(path, []byte(proof), 0600); err != nil {
		t.Fatal(err)
	}
	if err := worker.recoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	var receipts, attempts int
	if err := worker.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM outbox WHERE run_id=?1),
		(SELECT count(*) FROM git_push_attempts WHERE run_id=?1)`, run.ID).Scan(&receipts, &attempts); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || attempts != 1 {
		t.Fatalf("Git 读回恢复应保留原回执和读回意图，收到 %d 条回执和 %d 条意图", receipts, attempts)
	}
	if err := worker.clearGitAttempt(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	var protected int
	if err := worker.db.QueryRowContext(ctx, `SELECT count(*) FROM process_runs WHERE run_id=?`, run.ID).Scan(&protected); err != nil || protected != 0 {
		t.Fatalf("Git 读回结束后应删除终态保护记录：%d，%v", protected, err)
	}
}
