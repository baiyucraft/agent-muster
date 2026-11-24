package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

func TestInterruptedRunRecoversDurableGitReviewDiagnostics(t *testing.T) {
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	ctx := context.Background()
	const runID = "run-11111111-2222-3333-4444-555555555555"
	if _, err := worker.db.ExecContext(ctx, `INSERT INTO runs (id,task_id,directory_id,prompt,session_id,requested_session_id,state)
	VALUES (?, 'task-1', 'dir-1', '', '', '', 'claimed')`, runID); err != nil {
		t.Fatal(err)
	}
	if err := worker.markLaunching(ctx, runID); err != nil {
		t.Fatal(err)
	}
	attempt := runreport.ReviewAttempt{
		CandidateOID: strings.Repeat("a", 40), RepairIndex: 1, AgentKind: "codex", Status: "unverified",
		Diagnostics: &runreport.Diagnostics{ErrorKind: "interrupted", CaptureIncomplete: true}, LogInvocation: "review-1",
	}
	if err := worker.saveGitReview(ctx, runID, attempt); err != nil {
		t.Fatal(err)
	}
	if err := worker.recoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := worker.db.QueryRowContext(ctx, `SELECT payload FROM outbox WHERE run_id=?`, runID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event control.RunEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatal(err)
	}
	if event.Diagnostics == nil || len(event.Diagnostics.Reviews) != 1 || !reflect.DeepEqual(event.Diagnostics.Reviews[0], attempt) {
		t.Fatalf("中断回执缺少已保存的 Review 归属：%+v", event.Diagnostics)
	}
	var remaining int
	if err := worker.db.QueryRowContext(ctx, `SELECT count(*) FROM git_review_attempts WHERE run_id=?`, runID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("已提交的 outbox 回执应原子清理暂存诊断，remaining=%d err=%v", remaining, err)
	}
}

func TestRegisterDirectoryCreatesStableOpaqueBinding(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	worker, err := Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()

	first, err := worker.RegisterDirectory(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := worker.RegisterDirectory(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	expectedPath, err := canonicalDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || !strings.HasPrefix(first.ID, "dir-") || filepath.Clean(first.Path) != filepath.Clean(expectedPath) {
		t.Fatalf("directory binding was not stable: %#v %#v", first, second)
	}
	if len(first.Label) == 0 || len(first.Label) > 120 {
		t.Fatalf("unexpected label %q", first.Label)
	}
}

func TestRunReceiptIsDurableBeforeHubConfirmation(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	worker, err := Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	directory, err := worker.RegisterDirectory(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	run := control.Run{ID: "run-1", TaskID: "task-1", DirectoryID: directory.ID, Prompt: "list files"}
	if err := worker.saveClaim(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := worker.markLaunching(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := worker.stageEvent(ctx, run.ID, control.RunEvent{Kind: "started"}); err != nil {
		t.Fatal(err)
	}
	if err := worker.stageEvent(ctx, run.ID, control.RunEvent{Kind: "session", SessionID: "session-1"}); err != nil {
		t.Fatal(err)
	}
	if err := worker.stageEvent(ctx, run.ID, control.RunEvent{Kind: "completed", SessionID: "session-1", Text: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	latest, err := reopened.LatestRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.State != "completed" || latest.SessionID != "session-1" || !latest.Pending {
		t.Fatalf("run receipt was not durable: %#v", latest)
	}
	var count int
	if err := reopened.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE run_id=?`, run.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("got %d staged receipts, want 3", count)
	}
}

func TestInterruptedRunProducesOneRecoveryReceipt(t *testing.T) {
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	ctx := context.Background()
	const original = "MUSTER_APPROVAL_V1\n\x00"
	if _, err := worker.db.ExecContext(ctx, `
		INSERT INTO runs (id,task_id,directory_id,prompt,session_id,requested_session_id,state,text)
		VALUES ('run-1','task-1','dir-1','prompt','session-1','session-1','claimed',?)
	`, original); err != nil {
		t.Fatal(err)
	}
	if err := worker.markLaunching(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := worker.recoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := worker.recoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	var state, stored, payload string
	var receipts int
	if err := worker.db.QueryRowContext(ctx, `SELECT state,text FROM runs WHERE id='run-1'`).Scan(&state, &stored); err != nil {
		t.Fatal(err)
	}
	if err := worker.db.QueryRowContext(ctx, `SELECT count(*),max(payload) FROM outbox WHERE run_id='run-1'`).Scan(&receipts, &payload); err != nil {
		t.Fatal(err)
	}
	if state != "interrupted" || receipts != 1 {
		t.Fatalf("state=%q receipts=%d", state, receipts)
	}
	if stored != original || strings.Contains(payload, approvalMarker) {
		t.Fatalf("recovery did not keep the malformed reply local: %q, %q", stored, payload)
	}
}

func TestWorkerRetriesSameReceiptWithoutReexecutingRun(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	worker, err := Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RegisterDirectory(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	hub := &fakeHub{run: control.Run{ID: "run-1", TaskID: "task-1", DirectoryID: "dir-placeholder", Prompt: "work"}, failSequence: 3}
	directory, err := worker.Directories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hub.run.DirectoryID = directory[0].ID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var executions int
	done := make(chan error, 1)
	go func() {
		done <- worker.RunWithExecutor(ctx, hub, control.Identity{}, nil, func(_ context.Context, _, _, sessionID string, started func() error, session func(string) error) (string, error) {
			executions++
			if err := started(); err != nil {
				return "", err
			}
			if err := session("session-1"); err != nil {
				return "", err
			}
			return "done", nil
		})
	}()

	deadline := time.After(8 * time.Second)
	for {
		hub.mu.Lock()
		acknowledged := hub.acknowledged
		hub.mu.Unlock()
		if acknowledged {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("worker did not retry its persisted receipt")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if executions != 1 {
		t.Fatalf("run executed %d times, want once", executions)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	seq3Attempts := 0
	for _, sequence := range hub.receipts {
		if sequence == 3 {
			seq3Attempts++
		}
	}
	if seq3Attempts != 2 {
		t.Fatalf("terminal receipt delivered %d times, want one retry", seq3Attempts)
	}
}

func TestDrainWaitsForActiveRunAndStopsClaims(t *testing.T) {
	project := t.TempDir()
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(func() {
		cancel()
		_ = worker.Close()
	})
	directory, err := worker.RegisterDirectory(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	hub := &fakeHub{run: control.Run{ID: "run-1", TaskID: "task-1", DirectoryID: directory.ID, Prompt: "work"}}
	started, finish, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- worker.RunWithExecutor(ctx, hub, control.Identity{}, nil, func(ctx context.Context, _, _, _ string, markStarted func() error, _ func(string) error) (string, error) {
			if err := markStarted(); err != nil {
				return "", err
			}
			close(started)
			select {
			case <-finish:
				return "finished", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("run did not start")
	}

	probe, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if _, err := worker.Drain(probe); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drained while a run was active: %v", err)
	}
	close(finish)
	release, err := worker.Drain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	status, err := worker.LatestRun(ctx)
	if err != nil || status.State != "completed" || status.Pending {
		t.Fatalf("drained before completion was reported: %+v (%v)", status, err)
	}
	hub.mu.Lock()
	claims := hub.claimAttempts
	hub.mu.Unlock()
	worker.notify()
	time.Sleep(2 * idleInterval)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.claimAttempts != claims {
		t.Fatal("worker claimed new work while drained")
	}
}

type fakeHub struct {
	mu            sync.Mutex
	run           control.Run
	claimed       bool
	claimAttempts int
	acknowledged  bool
	failSequence  int64
	failed        bool
	receipts      []int64
}

func (*fakeHub) ReportDirectories(context.Context, control.Identity, []control.Directory) error {
	return nil
}

func (hub *fakeHub) ClaimRun(context.Context, control.Identity) (*control.Run, error) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.claimAttempts++
	if hub.claimed {
		return nil, nil
	}
	hub.claimed = true
	run := hub.run
	return &run, nil
}

func (hub *fakeHub) ReportRunEvent(_ context.Context, _ control.Identity, _ string, sequence int64, _ control.RunEvent) error {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.receipts = append(hub.receipts, sequence)
	if sequence == hub.failSequence && !hub.failed {
		hub.failed = true
		return errors.New("simulated lost response")
	}
	if sequence == 3 {
		hub.acknowledged = true
	}
	return nil
}
