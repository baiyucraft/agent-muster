package gitwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
)

const maxTestLog = 1 << 20

// TestResult 记录一次独立测试绑定的提交和本机日志。ExitCode 为空表示未取得可确认的退出码。
type TestResult struct {
	CandidateOID string
	ExitCode     *uint32
	// LogRef 是相对设备任务目录的日志引用，使用正斜杠分隔。
	LogRef       string
	LogTruncated bool
}

// Test 从候选提交创建独立工作区并执行测试。非零退出以结果返回，启动、取消或证据校验失败返回错误。
// 未确认进程树退出时保留现场，下一次测试使用独立的新目录。
func (work Workspace) Test(ctx context.Context, candidateOID string, command gitdelivery.Command) (result TestResult, err error) {
	if command.Program != work.test.Program || command.TimeoutSeconds != work.test.TimeoutSeconds || !slices.Equal(command.Args, work.test.Args) {
		return result, errors.New("Git 测试命令须与冻结的项目设置一致")
	}
	current, err := work.Candidate(ctx)
	if err != nil {
		return result, err
	}
	if current != candidateOID {
		return result, errors.New("待测提交已与 Git 修复工作区的候选不一致")
	}
	result.CandidateOID = candidateOID
	attempt, err := os.MkdirTemp(work.taskDir, "test-")
	if err != nil {
		return result, err
	}
	dir := filepath.Join(attempt, "checkout")
	if err := work.addWorktree(ctx, dir, candidateOID); err != nil {
		return result, fmt.Errorf("无法创建 Git 验证工作区: %w", err)
	}
	defer func() {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			errors.Is(err, runner.ErrProcessExitUnconfirmed) {
			return
		}
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if verifyErr := work.verifyWorktree(cleanCtx, dir); verifyErr != nil {
			err = errors.Join(err, verifyErr)
			return
		}
		_, cleanupErr := git(cleanCtx, work.root, "worktree", "remove", "--force", "--", dir)
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("无法清理 Git 验证工作区: %w", cleanupErr))
		}
	}()
	if err := work.verifyTestTree(ctx, dir, candidateOID, true); err != nil {
		return result, err
	}
	log, err := os.OpenFile(filepath.Join(attempt, "output.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, err
	}
	bounded := limitedOutput{destination: log, left: maxTestLog}
	testCtx, cancel := context.WithTimeout(ctx, time.Duration(command.TimeoutSeconds)*time.Second)
	defer cancel()
	code, runErr := runner.RunProjectCommandStreams(testCtx, dir, command.Program, command.Args, &bounded, &bounded)
	logErr := errors.Join(log.Sync(), log.Close())
	if logErr == nil {
		result.LogRef = filepath.Base(attempt) + "/output.log"
		result.LogTruncated = bounded.truncated
	}
	if runErr == nil {
		result.ExitCode = &code
	}
	if err := errors.Join(runErr, logErr); err != nil {
		return result, fmt.Errorf("无法完成 Git 候选测试: %w", err)
	}
	if code == 0 {
		return result, work.verifyTestTree(ctx, dir, candidateOID, false)
	}
	return result, nil
}

func (work Workspace) verifyTestTree(ctx context.Context, dir, candidateOID string, includeUntracked bool) error {
	if err := work.verifyWorktree(ctx, dir); err != nil {
		return err
	}
	head, err := worktreeHead(ctx, dir)
	if err != nil {
		return err
	}
	if head != candidateOID {
		return errors.New("Git 验证工作区的提交已改变")
	}
	return requireClean(ctx, dir, includeUntracked)
}
