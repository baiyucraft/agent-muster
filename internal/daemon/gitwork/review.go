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

// Review 从已验证候选创建独立工作区，并将目录、基线与候选提交 ID 传给 review。
// review 返回前须确认 CLI 进程树已退出。无法确认时返回 runner.ErrProcessExitUnconfirmed，保留本次工作区。
// 调用成功后重新核对审查工作区的提交、跟踪文件及修复工作区的候选。
func (work Workspace) Review(ctx context.Context, candidateOID string, review func(ctx context.Context, dir, baseOID, candidateOID string) error) (err error) {
	if review == nil {
		return errors.New("Git 独立审查缺少执行函数")
	}
	current, err := work.Candidate(ctx)
	if err != nil {
		return err
	}
	if current != candidateOID {
		return errors.New("待审查提交已与 Git 修复工作区的候选不一致")
	}
	attempt, err := os.MkdirTemp(work.taskDir, "review-")
	if err != nil {
		return fmt.Errorf("无法准备 Git 审查目录: %w", err)
	}
	dir := filepath.Join(attempt, "checkout")
	if err := work.addWorktree(ctx, dir, candidateOID); err != nil {
		// Git 可能已留下工作区登记。保留中途创建的目录，仅尝试移除空目录。
		_ = os.Remove(attempt)
		return fmt.Errorf("无法创建 Git 审查工作区: %w", err)
	}
	defer func() {
		if errors.Is(err, runner.ErrProcessExitUnconfirmed) {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if cleanupErr := work.removeReviewTree(cleanupCtx, attempt, dir); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("无法清理 Git 审查工作区: %w", cleanupErr))
		}
	}()
	if err := work.verifyTestTree(ctx, dir, candidateOID, true); err != nil {
		return err
	}
	if err := review(ctx, dir, work.BaseOID, candidateOID); err != nil {
		return err
	}
	if err := work.verifyTestTree(ctx, dir, candidateOID, false); err != nil {
		return fmt.Errorf("Git 审查工作区证据失效: %w", err)
	}
	current, err = work.Candidate(ctx)
	if err != nil {
		return err
	}
	if current != candidateOID {
		return errors.New("Git 候选在独立审查期间改变")
	}
	return nil
}

func (work Workspace) removeReviewTree(ctx context.Context, attempt, dir string) error {
	if err := work.verifyWorktree(ctx, dir); err != nil {
		return err
	}
	// 只移除通过路径及登记验证的本次工作区。审查产生的临时文件一并清理。
	if _, err := git(ctx, work.root, "worktree", "remove", "--force", "--", dir); err != nil {
		return err
	}
	return os.Remove(attempt)
}
