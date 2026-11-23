package gitwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

type candidateCommitError struct {
	message string
	cause   error
}

// Error 安全返回可上报的诊断摘要。原始输出只保留在设备本地。
func (err candidateCommitError) Error() string { return err.message }

// Unwrap 保留取消和进程树退出未确认等可由调用方识别的状态。
func (err candidateCommitError) Unwrap() error { return err.cause }

// runCandidateCommit 将 Git 和提交钩子的输出落盘，完成前同步文件。每次尝试保留独立日志。
func (work Workspace) runCandidateCommit(ctx context.Context, pathspec, message string) error {
	log, err := os.CreateTemp(work.taskDir, "commit-*.log")
	if err != nil {
		return candidateCommitError{message: "无法创建设备本地 Git 提交日志", cause: err}
	}
	const truncated = "\n[提交日志已达到大小上限]\n"
	bounded := limitedOutput{destination: log, left: maxTestLog - len(truncated)}
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	code, runErr := runner.RunProjectCommandStreams(commandCtx, work.FixDir, "git", []string{
		"--no-replace-objects", "--literal-pathspecs", "-c", "core.fsmonitor=false", "commit", "--quiet", "--only", "--message=" + message,
		"--pathspec-from-file=" + pathspec, "--pathspec-file-nul",
	}, &bounded, &bounded)
	var logErr error
	if bounded.truncated {
		_, logErr = log.WriteString(truncated)
	}
	logErr = errors.Join(logErr, log.Sync(), log.Close())
	if runErr != nil || code != 0 || logErr != nil {
		return candidateCommitError{
			message: fmt.Sprintf("无法完成 Git 候选提交，请查看设备任务目录中的 %s", filepath.Base(log.Name())),
			cause:   errors.Join(runErr, logErr),
		}
	}
	return nil
}
