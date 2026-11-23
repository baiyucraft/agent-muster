package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

type hub interface {
	ReportDirectories(context.Context, control.Identity, []control.Directory) error
	ClaimRun(context.Context, control.Identity) (*control.Run, error)
	ReportRunEvent(context.Context, control.Identity, string, int64, control.RunEvent) error
}

// Worker 管理本机运行、发件队列和任务领取，同一实例只启动一个执行循环。
type Worker struct {
	db   *sql.DB
	lock *os.File
	root string
	wake chan struct{}

	drainMu sync.Mutex
	drain   *drainRequest
}

type drainRequest struct {
	idle chan struct{}
}

// Drain 通知执行循环暂停领取任务，等待当前运行退出、待报回执获 Hub 确认。
// 等待期间继续上报。成功后由调用者调用返回的函数恢复领取，等待取消时自动恢复。
func (worker *Worker) Drain(ctx context.Context) (func(), error) {
	request := &drainRequest{idle: make(chan struct{})}
	worker.drainMu.Lock()
	if worker.drain != nil {
		worker.drainMu.Unlock()
		return nil, errors.New("设备正在等待空闲")
	}
	worker.drain = request
	worker.drainMu.Unlock()
	worker.notify()
	release := func() {
		worker.drainMu.Lock()
		if worker.drain == request {
			worker.drain = nil
		}
		worker.drainMu.Unlock()
		worker.notify()
	}
	select {
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	case <-request.idle:
		return release, nil
	}
}

func (worker *Worker) pendingDrain() *drainRequest {
	worker.drainMu.Lock()
	defer worker.drainMu.Unlock()
	return worker.drain
}

const (
	idleInterval = time.Second
	maxRetry     = 30 * time.Second
)

func (worker *Worker) notify() {
	select {
	case worker.wake <- struct{}{}:
	default:
	}
}

// Run 使用 dsh 串行执行任务，领取新任务前先上报待补报回执。
// 取消 ctx 会停止执行循环并等待正在运行的 CLI 退出。
func (worker *Worker) Run(ctx context.Context, client hub, identity control.Identity, onError func(error)) error {
	return worker.RunWithExecutor(ctx, client, identity, onError, runner.RunDSH)
}

// RunWithExecutor 以注入的执行函数运行 Run 的调度流程，执行函数负责响应 ctx 取消。
func (worker *Worker) RunWithExecutor(ctx context.Context, client hub, identity control.Identity, onError func(error), execute func(context.Context, string, string, string, func() error, func(string) error) (string, error)) error {
	return worker.run(ctx, client, identity, onError, nil, nil, func(ctx context.Context, _ string, dir, prompt, session string, _ []string, started func() error, saveSession func(string) error) (string, error) {
		return execute(ctx, dir, prompt, session, started, saveSession)
	})
}

// RunWithPeer 启动本机派活接口和设备交接，按任务绑定选择 CLI。
// 取消 ctx 后等待执行与交接循环退出，再关闭本机派活接口。
func (worker *Worker) RunWithPeer(ctx context.Context, client *control.Client, identity control.Identity, config PeerConfig, onError func(error)) error {
	return worker.runWithPeer(ctx, client, identity, config, onError, executeAgent)
}

func (worker *Worker) runWithPeer(ctx context.Context, client collaborationHub, identity control.Identity, config PeerConfig, onError func(error), execute agentExecutor) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	peer, err := openPeer(ctx, worker, client, identity, config)
	if err != nil {
		if peer == nil {
			peer = &collaboration{worker: worker, client: client, identity: identity}
		}
		peerErr := fmt.Errorf("无法启动设备互联: %w", err)
		if report := onError; report != nil {
			onError = func(runErr error) { report(errors.Join(peerErr, runErr)) }
		}
	}
	defer peer.close()
	service, err := startHelper(ctx, peer)
	if err != nil {
		return err
	}
	defer service.listener.Close()
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		peer.Run(ctx, onError)
	}()
	defer func() {
		cancel()
		<-peerDone
	}()
	return worker.run(ctx, client, identity, onError, peer, service, execute)
}

type agentExecutor func(context.Context, string, string, string, string, []string, func() error, func(string) error) (string, error)

func executeAgent(ctx context.Context, kind, directory, prompt, sessionID string, env []string, started func() error, session func(string) error) (string, error) {
	switch kind {
	case "", "dsh":
		return runner.RunDSHWithEnvironment(ctx, directory, prompt, sessionID, env, started, session)
	case "codex":
		return runner.RunCodex(ctx, directory, prompt, sessionID, env, started, session)
	default:
		return "", errors.New("该设备端不支持此 Agent")
	}
}

func (worker *Worker) run(ctx context.Context, client hub, identity control.Identity, onError func(error), peer *collaboration, service *localHelper, execute agentExecutor) error {
	ctx, cancel := context.WithCancel(ctx)
	var active <-chan error
	defer func() {
		cancel()
		if active != nil {
			<-active
		}
	}()
	if err := worker.recoverInterrupted(ctx); err != nil {
		return err
	}

	var lastDirectories string
	delay := idleInterval
	for ctx.Err() == nil {
		if active != nil {
			select {
			case err := <-active:
				active = nil
				if err != nil {
					return err
				}
			default:
			}
		}
		drain := worker.pendingDrain()
		err := worker.exchange(ctx, client, identity, &lastDirectories, active != nil || drain != nil, peer)
		if errors.Is(err, control.ErrUnauthorized) {
			return err
		}
		if APIUnavailable(err) {
			if onError != nil {
				onError(err)
			}
			wait := time.NewTimer(maxRetry)
			select {
			case <-ctx.Done():
			case <-wait.C:
			}
			wait.Stop()
			continue
		}
		if err == nil && drain != nil && active == nil {
			pending, err := worker.nextClaim(ctx)
			if err != nil {
				return err
			}
			if pending == nil {
				select {
				case <-drain.idle:
				default:
					close(drain.idle)
				}
			}
		}
		if err == nil && active == nil && drain == nil {
			claim, err := worker.nextClaim(ctx)
			if err != nil {
				return err
			}
			if claim != nil {
				outcome := make(chan error, 1)
				go func(run control.Run) {
					outcome <- worker.execute(ctx, run, peer, service, execute)
					worker.notify()
				}(*claim)
				active = outcome
			}
		}
		if onError != nil {
			onError(err)
		}
		wait := idleInterval
		if err != nil {
			wait = delay
			delay = min(delay*2, maxRetry)
		} else {
			delay = idleInterval
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-worker.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
	return nil
}

func (worker *Worker) exchange(ctx context.Context, client hub, identity control.Identity, lastDirectories *string, running bool, peer *collaboration) error {
	if peer != nil {
		if err := peer.flushDelegations(ctx); err != nil {
			return err
		}
	}
	if err := worker.flushOutbox(ctx, client, identity, peer); err != nil {
		return err
	}
	directories, err := worker.Directories(ctx)
	if err != nil {
		return err
	}
	remote := make([]control.Directory, 0, len(directories))
	for _, directory := range directories {
		remote = append(remote, control.Directory{ID: directory.ID, Label: directory.Label})
	}
	payload, err := json.Marshal(remote)
	if err != nil {
		return err
	}
	if string(payload) != *lastDirectories {
		if err := client.ReportDirectories(ctx, identity, remote); err != nil {
			return err
		}
		*lastDirectories = string(payload)
	}
	if running {
		return nil
	}
	pending, err := worker.nextClaim(ctx)
	if err != nil || pending != nil {
		return err
	}
	run, err := client.ClaimRun(ctx, identity)
	if err != nil || run == nil {
		return err
	}
	return worker.saveClaim(ctx, *run)
}

func (worker *Worker) flushOutbox(ctx context.Context, client hub, identity control.Identity, peer *collaboration) error {
	for {
		var id string
		var sequence int64
		var payload string
		err := worker.db.QueryRowContext(ctx, `SELECT run_id,sequence,payload FROM outbox ORDER BY rowid LIMIT 1`).Scan(&id, &sequence, &payload)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		var event control.RunEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return err
		}
		if peer != nil && (event.Kind == "completed" || event.Kind == "failed") {
			for _, transfer := range event.TransferIDs {
				if _, err := peer.publishTransfer(ctx, transfer); err != nil {
					return err
				}
			}
		}
		if err := client.ReportRunEvent(ctx, identity, id, sequence, event); err != nil {
			return err
		}
		if _, err := worker.db.ExecContext(ctx, `DELETE FROM outbox WHERE run_id=? AND sequence=?`, id, sequence); err != nil {
			return err
		}
	}
}

func (worker *Worker) execute(ctx context.Context, run control.Run, peer *collaboration, service *localHelper, execute agentExecutor) error {
	if err := worker.markLaunching(ctx, run.ID); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	runCtx, abandon := context.WithCancelCause(ctx)
	defer abandon(nil)
	runCtx, readDiagnostics := worker.captureRun(runCtx, run.ID)
	var leaseErr error
	if run.Lease != "" {
		leaseErr = startLease(runCtx, run, peer, abandon)
	}
	if leaseErr == nil {
		startRunControl(runCtx, run, peer, abandon)
	}
	sessionID := run.SessionID
	event := control.RunEvent{Kind: "failed", SessionID: sessionID, Generation: run.Generation, Lease: run.Lease}
	err := errors.Join(leaseErr, runCtx.Err())
	var dir string
	if err == nil {
		dir, err = worker.resolveDirectory(runCtx, run.DirectoryID)
	}
	if err == nil {
		if run.GitPolicy != nil {
			event, err = worker.invokeGit(runCtx, run, service, dir, execute, event)
		} else {
			event, err = worker.invoke(runCtx, run, service, dir, execute, event)
		}
	}
	if errors.Is(err, runner.ErrProcessExitUnconfirmed) {
		// 退出尚未确认时继续保留本地活动记录，等待设备运行恢复流程核对。
		return err
	}
	if errors.Is(context.Cause(runCtx), errRunStopRequested) && err != nil {
		event.Kind = "stopped"
		event.Text = "设备已停止本轮执行。"
		err = nil
	}
	if err != nil {
		if event.Text != "" {
			event.Text = err.Error() + "\n\n" + event.Text
		} else {
			event.Text = err.Error()
		}
	}
	if run.DiagnosticsSupported {
		event.Diagnostics = readDiagnostics()
		if event.Diagnostics != nil && (event.Kind == "stopped" || event.Kind == "interrupted" && runCtx.Err() != nil) {
			event.Diagnostics.ErrorKind = "interrupted"
		}
		if event.Diagnostics == nil && err != nil {
			event.Diagnostics = &runreport.Diagnostics{ErrorKind: "setup"}
		}
		if event.Diagnostics != nil && event.Diagnostics.ErrorKind == "" && err != nil {
			event.Diagnostics.ErrorKind = "output"
		}
		if !event.Diagnostics.Valid() {
			event.Diagnostics = nil
		}
	}
	// Persist terminal state and result body together, including on shutdown.
	persist, stopPersist := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopPersist()
	if run.ParentDeviceID != "" {
		body := []byte(event.Text)
		event.Text = ""
		if peer != nil && peer.enabled() && event.Kind == "completed" && len(body) > 0 && len(body) <= control.MaxPeerBody {
			manifest := newManifest(run, transferKey(run.ID, "result", "final"), run.ParentDeviceID, "result.txt", "result", body)
			ids, err := worker.publishedReplies(persist, run.ID, run.ParentDeviceID)
			if err != nil {
				return err
			}
			event.TransferIDs = append([]string{manifest.ID}, ids...)
			return worker.stageEventWithResult(persist, run.ID, event, manifest, body)
		}
		if event.Kind == "completed" {
			event.Kind = "failed"
		}
	}
	event.Text = trimUTF8(event.Text, 12000)
	return worker.stageEvent(persist, run.ID, event)
}

func (worker *Worker) invoke(ctx context.Context, run control.Run, service *localHelper, dir string, execute agentExecutor, event control.RunEvent) (control.RunEvent, error) {
	prompt, err := worker.preparePrompt(ctx, run, dir, service)
	if err != nil {
		return event, err
	}
	env, finish, err := service.setRun(ctx, run, dir)
	if err != nil {
		return event, err
	}
	defer finish()
	sessionID := run.SessionID
	var didStart bool
	event.Text, err = execute(ctx, run.AgentKind, dir, prompt, sessionID, env, func() error {
		started := control.RunEvent{Kind: "started", SessionID: sessionID, Generation: run.Generation, Lease: run.Lease}
		if err := worker.stageEvent(ctx, run.ID, started); err != nil {
			return err
		}
		if run.Lease != "" {
			if err := worker.awaitReceipts(ctx, run.ID); err != nil {
				return err
			}
		}
		didStart = true
		return nil
	}, func(id string) error {
		persist, stopPersist := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stopPersist()
		if err := worker.stageEvent(persist, run.ID, control.RunEvent{Kind: "session", SessionID: id, Generation: run.Generation, Lease: run.Lease}); err != nil {
			return err
		}
		sessionID = id
		return nil
	})
	event.SessionID = sessionID
	switch {
	case didStart && ctx.Err() != nil:
		event.Kind = "interrupted"
		err = errors.Join(context.Cause(ctx), err)
	case err == nil:
		event.Kind = "completed"
		wait, waitErr := worker.requestedWait(ctx, run.ID)
		if waitErr != nil {
			event.Kind = "failed"
			err = waitErr
			return event, err
		}
		if run.ApprovalSupported {
			err = worker.finishApproval(ctx, run, dir, execute, &event, wait.Kind == "waiting")
		}
		if event.Kind == "completed" && wait.Kind == "waiting" {
			event.Kind, event.WaitForTaskIDs, event.TransferIDs = wait.Kind, wait.WaitForTaskIDs, wait.TransferIDs
			event.Text = ""
		}
	}
	return event, err
}

func (worker *Worker) awaitReceipts(ctx context.Context, runID string) error {
	for ctx.Err() == nil {
		var pending bool
		if err := worker.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM outbox WHERE run_id=?)", runID).Scan(&pending); err != nil {
			return err
		}
		if !pending {
			return nil
		}
		sleepContext(ctx, 100*time.Millisecond)
	}
	return context.Cause(ctx)
}

func (worker *Worker) preparePrompt(ctx context.Context, run control.Run, directory string, service *localHelper) (string, error) {
	prompt := run.Prompt
	if run.PromptTransferID != "" {
		body, err := worker.readPrompt(ctx, run.PromptTransferID)
		if err != nil {
			return "", fmt.Errorf("无法读取本机已接收的提示词: %w", err)
		}
		if prompt != "" {
			prompt += "\n\n"
		}
		prompt += string(body)
	}
	var notes strings.Builder
	for _, attachment := range run.Attachments {
		if attachment.Kind == "prompt" {
			continue
		}
		file, err := worker.exportTransfer(ctx, directory, attachment)
		if err != nil {
			return "", fmt.Errorf("无法准备本机附件 %s: %w", attachment.ID, err)
		}
		fmt.Fprintf(&notes, "\n- %s (%s): %q", attachment.ID, attachment.Kind, file)
	}
	if notes.Len() != 0 {
		prompt += "\n\n可在当前目录查看以下已接收的附件或子任务结果：" + notes.String()
	}
	if service != nil && service.peer.enabled() {
		prompt += collaborationInstructions()
	}
	if run.ApprovalSupported {
		prompt += approvalInstructions
	}
	return prompt, nil
}

func (worker *Worker) resolveDirectory(ctx context.Context, id string) (string, error) {
	var dir string
	if err := worker.db.QueryRowContext(ctx, `SELECT path FROM directories WHERE id=?`, id).Scan(&dir); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errors.New("这台设备尚未登记该工作目录")
		}
		return "", err
	}
	current, err := canonicalDirectory(dir)
	if err != nil || filepath.Clean(current) != dir {
		return "", errors.New("已登记的工作目录不可用或已指向其他位置")
	}
	return dir, nil
}

// APIUnavailable 判断 Hub 是否缺少任务派发接口，供桌面端在该情况下继续显示设备清单。
func APIUnavailable(err error) bool {
	var status *control.HTTPError
	return errors.As(err, &status) && (status.Status == http.StatusServiceUnavailable || status.Status == http.StatusNotFound)
}
