package work

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

var processProofName = regexp.MustCompile(`^exit-[0-9a-f]{32}\.proof$`)

// checkProcessRecovery 核对旧运行状态和本机所有未结清的命令，含终态运行的 Git 读回。
func (worker *Worker) checkProcessRecovery(ctx context.Context) error {
	if worker.processProofs == nil {
		return nil
	}
	var legacy bool
	err := worker.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM runs r LEFT JOIN run_claims c ON c.id=r.id
		WHERE (r.state IN ('starting','started') OR EXISTS (
			SELECT 1 FROM git_push_attempts g WHERE g.run_id=r.id)) AND NOT EXISTS (
			SELECT 1 FROM process_runs p WHERE p.run_id=r.id
			AND p.generation=COALESCE(json_extract(CAST(c.data AS TEXT),'$.generation'),0)))`).Scan(&legacy)
	if err != nil {
		return err
	}
	if legacy {
		return errors.Join(runner.ErrProcessExitUnconfirmed, errors.New("本机遗留运行或 Git 回读缺少匹配的进程保护记录"))
	}
	return worker.checkProcessLaunches(ctx)
}

// checkProcessLaunches 成功后才允许启动下一条项目命令或提交运行终态。
// 未授权启动的记录可以清除。已授权记录必须与当前运行世代匹配并持有精确凭据。
func (worker *Worker) checkProcessLaunches(ctx context.Context) error {
	if worker.processProofs == nil {
		return nil
	}
	var name string
	var armed, matches bool
	err := worker.db.QueryRowContext(ctx, `SELECT p.proof_name,p.armed,
		p.generation=COALESCE(json_extract(CAST(c.data AS TEXT),'$.generation'),0)
		FROM process_launches p LEFT JOIN run_claims c ON c.id=p.run_id`).Scan(&name, &armed, &matches)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !processProofName.MatchString(name) {
		return errors.Join(runner.ErrProcessExitUnconfirmed, errors.New("本机进程退出凭据名称无效"))
	}
	path := filepath.Join(worker.processProofs.Name(), name)
	if armed {
		if !matches {
			return errors.Join(runner.ErrProcessExitUnconfirmed, errors.New("本机进程退出凭据与运行代次不匹配"))
		}
		confirmed, err := runner.ProcessExitConfirmed(path)
		if err != nil {
			// 设备界面不显示凭据路径和随机名称。
			return errors.Join(runner.ErrProcessExitUnconfirmed, errors.New("无法读取本机进程退出凭据"))
		}
		if !confirmed {
			return runner.ErrProcessExitUnconfirmed
		}
	}
	result, err := worker.db.ExecContext(ctx, `DELETE FROM process_launches WHERE proof_name=? AND armed=?`, name, armed)
	if err := requireProcessRow(result, err); err != nil {
		return errors.Join(runner.ErrProcessExitUnconfirmed, err)
	}
	// 先提交 SQLite，再删除证据。崩溃最多留下文件，不会使已确认的启动回到未知。
	_ = os.Remove(path)
	return nil
}

// withProcessRecovery 把运行世代传到代理、项目测试、Git 及其清理命令共享的进程运行器。
func (worker *Worker) withProcessRecovery(ctx context.Context, runID string, generation int64, gitRecovery bool) context.Context {
	if worker.processProofs == nil {
		return ctx
	}
	return runner.WithProcessProof(ctx, func() (string, func() error, error) {
		// Git 清理命令可以使用独立于运行取消的上下文。持久化独立限时，仍在启动前完成。
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return worker.reserveProcessLaunch(persist, runID, generation, gitRecovery)
	})
}

// reserveProcessLaunch 在任何项目进程创建前记录随机凭据名。
// arm 在守护进程确认持有 Job 后提交，此后即使进程尚未实际创建，也按已获授权处理。
func (worker *Worker) reserveProcessLaunch(ctx context.Context, runID string, generation int64, gitRecovery bool) (string, func() error, error) {
	if err := worker.checkProcessLaunches(ctx); err != nil {
		return "", nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, err
	}
	name := "exit-" + hex.EncodeToString(nonce[:]) + ".proof"
	result, err := worker.db.ExecContext(ctx, `INSERT INTO process_launches (proof_name,run_id,generation)
		SELECT ?1,r.id,?3 FROM runs r LEFT JOIN run_claims c ON c.id=r.id
		WHERE r.id=?2 AND COALESCE(json_extract(CAST(c.data AS TEXT),'$.generation'),0)=?3 AND (
			(r.state IN ('starting','started') AND EXISTS (
				SELECT 1 FROM process_runs p WHERE p.run_id=r.id AND p.generation=?3))
			OR (?4 AND EXISTS (SELECT 1 FROM git_push_attempts p WHERE p.run_id=r.id)))`,
		name, runID, generation, gitRecovery)
	if err := requireProcessRow(result, err); err != nil {
		return "", nil, err
	}
	arm := func() error {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		result, err := worker.db.ExecContext(persist, `UPDATE process_launches SET armed=1 WHERE proof_name=? AND armed=0`, name)
		return requireProcessRow(result, err)
	}
	return filepath.Join(worker.processProofs.Name(), name), arm, nil
}

func requireProcessRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return errors.New("本机进程保护记录与运行代次不匹配")
	}
	return err
}

// recoverProcessState 在退出凭据尚未落盘时保留执行名额，并向设备界面持续报告。
func (worker *Worker) recoverProcessState(ctx context.Context, onError func(error), blocked bool) error {
	for ctx.Err() == nil {
		err := worker.recoverInterrupted(ctx)
		if !errors.Is(err, runner.ErrProcessExitUnconfirmed) {
			if err == nil && blocked && onError != nil {
				onError(nil)
			}
			return err
		}
		blocked = true
		if onError != nil {
			onError(err)
		}
		sleepContext(ctx, idleInterval)
	}
	return ctx.Err()
}
