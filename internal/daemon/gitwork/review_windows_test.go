package gitwork

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

func TestReviewChecksCandidateAndRemovesTheConfirmedWorktree(t *testing.T) {
	root, task, base := fixtureRepo(t)
	work, candidate := fixtureCandidate(t, root, task, fixturePolicy())
	var reviewDir string
	if err := work.Review(t.Context(), candidate, func(ctx context.Context, dir, gotBase, gotCandidate string) error {
		reviewDir = dir
		if gotBase != base || gotCandidate != candidate || overlaps(dir, work.FixDir) || localGit(t, dir, "rev-parse", "HEAD") != candidate {
			t.Fatalf("审查目录或提交不匹配，目录 %q，基线 %s，候选 %s", dir, gotBase, gotCandidate)
		}
		writeFixture(t, filepath.Join(dir, "review.tmp"), "审查过程生成的文件")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertReviewRemoval(t, reviewDir)
	if current, err := work.Candidate(t.Context()); current != candidate || err != nil {
		t.Fatalf("审查改变了修复候选，当前 %s，错误 %v", current, err)
	}
	if err := work.Review(t.Context(), base, func(context.Context, string, string, string) error {
		t.Fatal("过期候选启动了审查")
		return nil
	}); err == nil {
		t.Fatal("审查接受了过期候选")
	}
}

func TestReviewRejectsChangesToTheCheckout(t *testing.T) {
	root, task, _ := fixtureRepo(t)
	work, candidate := fixtureCandidate(t, root, task, fixturePolicy())
	for _, test := range []struct {
		name   string
		change func(t *testing.T, dir string)
	}{
		{"tracked files", func(t *testing.T, dir string) { writeFixture(t, filepath.Join(dir, "data.txt"), "changed by reviewer") }},
		{"detached HEAD", func(t *testing.T, dir string) { localGit(t, dir, "checkout", "--detach", work.BaseOID) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reviewDir string
			err := work.Review(t.Context(), candidate, func(_ context.Context, dir, _, _ string) error {
				reviewDir = dir
				test.change(t, dir)
				return nil
			})
			if err == nil {
				t.Fatal("审查接受了已变更的工作区")
			}
			assertReviewRemoval(t, reviewDir)
		})
	}
}

func TestReviewRetainsUnconfirmedProcessWorktreeAndCanRetry(t *testing.T) {
	root, task, _ := fixtureRepo(t)
	work, candidate := fixtureCandidate(t, root, task, fixturePolicy())
	var heldDir string
	err := work.Review(t.Context(), candidate, func(_ context.Context, dir, _, _ string) error {
		heldDir = dir
		return errors.Join(context.Canceled, runner.ErrProcessExitUnconfirmed)
	})
	if !errors.Is(err, runner.ErrProcessExitUnconfirmed) {
		t.Fatalf("未保留进程退出状态，错误 %v", err)
	}
	if _, err := os.Stat(heldDir); err != nil {
		t.Fatalf("退出未确认时应保留审查目录，错误 %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var retryDir string
	err = work.Review(ctx, candidate, func(_ context.Context, dir, _, _ string) error {
		retryDir = dir
		if dir == heldDir {
			t.Fatal("重试复用了可能仍在使用的审查目录")
		}
		cancel()
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("审查取消返回 %v", err)
	}
	assertReviewRemoval(t, retryDir)
}

func assertReviewRemoval(t *testing.T, dir string) {
	t.Helper()
	if dir == "" {
		t.Fatal("审查没有打开独立工作区")
	}
	if _, err := os.Stat(filepath.Dir(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("已确认退出的审查目录未移除，错误 %v", err)
	}
}
