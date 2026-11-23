package gitwork

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommitSelectedFixWorkspace 覆盖普通和 Unicode 文件、删除、原仓库脏文件及设备收件目录。
func TestCommitSelectedFixWorkspace(t *testing.T) {
	root, task, _ := fixtureRepo(t)
	writeFixture(t, filepath.Join(root, "old.txt"), "remove\n")
	localGit(t, root, "add", "--", "old.txt")
	localGit(t, root, "commit", "-m", "second baseline")
	localGit(t, root, "branch", "-f", "delivery-base", "HEAD")
	base := localGit(t, root, "rev-parse", "HEAD")
	writeFixture(t, filepath.Join(root, "data.txt"), "user edit\n")
	work := prepareCommitFixture(t, root, task)
	if head, err := work.FixHead(t.Context()); err != nil || head != base {
		t.Fatalf("修复基线为 %s，错误为 %v", head, err)
	}
	writeFixture(t, filepath.Join(work.FixDir, "data.txt"), "candidate\n")
	writeFixture(t, filepath.Join(work.FixDir, "新增 [文件].txt"), "new\n")
	if err := os.Remove(filepath.Join(work.FixDir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(work.FixDir, ".muster-inbox")
	if err := os.Mkdir(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(inbox, "attachment.txt"), "local only\n")
	selected := []string{"data.txt", "old.txt", "新增 [文件].txt"}
	candidate, err := work.CommitSelected(t.Context(), base, selected, "fix: 【项目】修正示例")
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := work.Candidate(t.Context()); err != nil || actual != candidate || actual == base {
		t.Fatalf("候选为 %s，预期为 %s，错误为 %v", actual, candidate, err)
	}
	if parent := localGit(t, work.FixDir, "rev-parse", "HEAD^"); parent != base {
		t.Fatalf("候选父提交为 %s，预期为 %s", parent, base)
	}
	if message := localGit(t, work.FixDir, "log", "-1", "--format=%s"); message != "fix: 【项目】修正示例" {
		t.Fatalf("Git 提交说明为 %q", message)
	}
	assertFixture(t, filepath.Join(root, "data.txt"), "user edit\n")
	assertFixture(t, filepath.Join(inbox, "attachment.txt"), "local only\n")
	if original := localGit(t, root, "rev-parse", "HEAD"); original != base {
		t.Fatalf("绑定仓库 HEAD 被修改为 %s", original)
	}
}

// TestCommitSelectedRejectsInvalidManifest 确认拒绝清单后保留文件、暂存区和会话起始提交。
func TestCommitSelectedRejectsInvalidManifest(t *testing.T) {
	root, task, base := fixtureRepo(t)
	localGit(t, root, "config", "core.excludesfile", filepath.Join(root, "ignore-config"))
	writeFixture(t, filepath.Join(root, "ignore-config"), "ignored.txt\n")
	work := prepareCommitFixture(t, root, task)
	writeFixture(t, filepath.Join(work.FixDir, "data.txt"), "changed\n")
	writeFixture(t, filepath.Join(work.FixDir, "ignored.txt"), "not a project change\n")
	for _, tc := range []struct {
		name  string
		paths []string
	}{
		{"unknown", []string{"data.txt", "unknown.txt"}},
		{"ignored", []string{"data.txt", "ignored.txt"}},
		{"absolute", []string{filepath.ToSlash(filepath.Join(root, "data.txt"))}},
		{"parent", []string{"../data.txt"}},
		{"pathspec", []string{":(glob)*"}},
		{"directory", []string{"nested"}},
		{"inbox", []string{".MUSTER-INBOX/private.txt"}},
		{"git", []string{"nested/.Git/config"}},
		{"duplicate", []string{"data.txt", "DATA.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := work.CommitSelected(t.Context(), base, tc.paths, "invalid fixture"); err == nil {
				t.Fatal("无效清单生成了候选提交")
			}
		})
	}
	if _, err := work.CommitSelected(t.Context(), strings.Repeat("0", 40), []string{"data.txt"}, "stale"); err == nil {
		t.Fatal("过期 HEAD 生成了候选提交")
	}
	if err := os.Mkdir(filepath.Join(work.FixDir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(work.FixDir, "nested", "unselected.txt"), "unrelated\n")
	if _, err := work.CommitSelected(t.Context(), base, []string{"data.txt"}, "incomplete"); err == nil {
		t.Fatal("遗漏工作区文件的清单生成了候选提交")
	}
	if current := localGit(t, work.FixDir, "rev-parse", "HEAD"); current != base {
		t.Fatalf("拒绝清单后 HEAD 为 %s", current)
	}
	if staged := localGit(t, work.FixDir, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("拒绝清单改变了暂存区 %q", staged)
	}
	assertFixture(t, filepath.Join(work.FixDir, "data.txt"), "changed\n")
	assertFixture(t, filepath.Join(work.FixDir, "nested", "unselected.txt"), "unrelated\n")
	localGit(t, work.FixDir, "add", "--", "data.txt")
	if _, err := work.CommitSelected(t.Context(), base, []string{"data.txt", "nested/unselected.txt"}, "staged"); err == nil {
		t.Fatal("已有暂存内容时生成了候选提交")
	}
	if staged := localGit(t, work.FixDir, "diff", "--cached", "--name-only"); staged != "data.txt" {
		t.Fatalf("拒绝清单改变了既有暂存内容 %q", staged)
	}
}

// TestCommitSelectedKeepsHooksAndCanRetry 验证项目钩子保留审批作用，失败后允许重试新增文件。
func TestCommitSelectedKeepsHooksAndCanRetry(t *testing.T) {
	root, task, base := fixtureRepo(t)
	hooks := t.TempDir()
	localGit(t, root, "config", "core.hooksPath", filepath.ToSlash(hooks))
	hook := filepath.Join(hooks, "pre-commit")
	writeFixture(t, hook, "#!/bin/sh\necho fixture-stdout\necho fixture-stderr >&2\nexit 1\n")
	work := prepareCommitFixture(t, root, task)
	writeFixture(t, filepath.Join(work.FixDir, "new.txt"), "a new file\n")
	if _, err := work.CommitSelected(t.Context(), base, []string{"new.txt"}, "hook fixture"); err == nil {
		t.Fatal("仓库钩子拒绝后仍创建了候选提交")
	} else {
		logs, globErr := filepath.Glob(filepath.Join(work.taskDir, "commit-*.log"))
		if globErr != nil || len(logs) != 1 {
			t.Fatalf("预期保留一份提交日志，得到 %d 份，错误为 %v", len(logs), globErr)
		}
		data, readErr := os.ReadFile(logs[0])
		if readErr != nil || !strings.Contains(string(data), "fixture-stdout") || !strings.Contains(string(data), "fixture-stderr") {
			t.Fatalf("设备提交日志未保留完整钩子诊断 %q，错误为 %v", data, readErr)
		}
		if !strings.Contains(err.Error(), filepath.Base(logs[0])) || strings.Contains(err.Error(), "fixture-") {
			t.Fatalf("提交错误未使用日志摘要 %q", err)
		}
	}
	if head := localGit(t, work.FixDir, "rev-parse", "HEAD"); head != base {
		t.Fatalf("钩子拒绝后 HEAD 为 %s", head)
	}
	writeFixture(t, hook, "#!/bin/sh\nexit 0\n")
	candidate, err := work.CommitSelected(t.Context(), base, []string{"new.txt"}, "hook fixture")
	if err != nil || candidate == base {
		t.Fatalf("钩子放行后重试得到 %s，错误为 %v", candidate, err)
	}
}

// TestCommitSelectedRejectsJunction 验证包含重解析点的路径不能通过文件清单验证。
func TestCommitSelectedRejectsJunction(t *testing.T) {
	root, task, base := fixtureRepo(t)
	work := prepareCommitFixture(t, root, task)
	if err := os.Mkdir(filepath.Join(work.FixDir, "target"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(work.FixDir, "target", "file.txt"), "new\n")
	cmd := exec.CommandContext(t.Context(), "cmd.exe", "/d", "/c", "mklink", "/J", "directory-link", "target")
	cmd.Dir = work.FixDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("当前文件系统无法创建目录连接 %v %s", err, output)
	}
	openRoot, err := os.OpenRoot(work.FixDir)
	if err != nil {
		t.Fatal(err)
	}
	defer openRoot.Close()
	if err := checkCommitPath(openRoot, "directory-link/file.txt", false); err == nil {
		t.Fatal("接受了经过重解析点的路径")
	}
	if _, err := work.CommitSelected(t.Context(), base, []string{"directory-link/file.txt", "target/file.txt"}, "junction fixture"); err == nil {
		t.Fatal("含重解析点的清单生成了候选提交")
	}
}

// TestCommitSelectedRecoversVerifiedHead 重放已通过原清单核对的 HEAD，并保留超出清单的本机候选。
func TestCommitSelectedRecoversVerifiedHead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
		valid bool
	}{
		{"same paths", []string{"data.txt"}, true},
		{"changed paths", []string{"unexpected.txt"}, false},
		{"wrong case", []string{"DATA.txt"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, task, base := fixtureRepo(t)
			work := prepareCommitFixture(t, root, task)
			if err := work.recordCommit(commitRecord{Parent: base, Paths: tc.paths}); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, filepath.Join(work.FixDir, "data.txt"), "candidate\n")
			localGit(t, work.FixDir, "commit", "-am", "interrupted candidate")
			candidate := localGit(t, work.FixDir, "rev-parse", "HEAD")
			reopened := prepareCommitFixture(t, root, task)
			head, err := reopened.FixHead(t.Context())
			if tc.valid && (err != nil || head != candidate) {
				t.Fatalf("恢复出的候选为 %s，预期为 %s，错误为 %v", head, candidate, err)
			}
			if !tc.valid && err == nil {
				t.Fatal("接受了超出清单的本机候选")
			}
			if actual := localGit(t, work.FixDir, "rev-parse", "HEAD"); actual != candidate {
				t.Fatalf("核对改变了本机 HEAD %s，预期为 %s", actual, candidate)
			}
			_, err = os.Stat(filepath.Join(work.taskDir, commitRecordName))
			if tc.valid && !os.IsNotExist(err) || !tc.valid && err != nil {
				t.Fatalf("中断记录的保留状态与核对结果不符 %v", err)
			}
			if tc.valid {
				if head, err := reopened.FixHead(t.Context()); err != nil || head != candidate {
					t.Fatalf("再次核对候选为 %s，预期为 %s，错误为 %v", head, candidate, err)
				}
			}
		})
	}
}

// TestCommitSelectedPinsAcceptedHead 验证正常返修和恢复，并拒绝未经 daemon 接受的提交。
func TestCommitSelectedPinsAcceptedHead(t *testing.T) {
	root, task, base := fixtureRepo(t)
	work := prepareCommitFixture(t, root, task)
	selected := []string{"data.txt"}
	file := filepath.Join(work.FixDir, selected[0])
	writeFixture(t, file, "first\n")
	localGit(t, work.FixDir, "commit", "-am", "agent commit")
	if _, err := work.FixHead(t.Context()); err == nil {
		t.Fatal("接受了没有 daemon 记录的提交")
	}
	localGit(t, work.FixDir, "checkout", "--detach", base)
	writeFixture(t, file, "first\n")
	first, err := work.CommitSelected(t.Context(), base, selected, "first daemon candidate")
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, file, "second\n")
	second, err := work.CommitSelected(t.Context(), first, selected, "second daemon candidate")
	if err != nil {
		t.Fatal(err)
	}
	// 模拟已写入候选 ID、尚未清除待核对记录时退出。
	if err := work.recordCommit(commitRecord{Parent: first, Paths: selected}); err != nil {
		t.Fatal(err)
	}
	if current, err := work.FixHead(t.Context()); err != nil || current != second {
		t.Fatalf("从待核对记录恢复候选为 %s，预期为 %s，错误为 %v", current, second, err)
	}
	localGit(t, work.FixDir, "checkout", "--detach", first)
	if _, err := work.FixHead(t.Context()); err == nil {
		t.Fatal("接受了回退到旧提交的工作区")
	}
}

func prepareCommitFixture(t *testing.T, root, task string) Workspace {
	t.Helper()
	work, err := Prepare(t.Context(), root, task, fixturePolicy())
	if err != nil {
		t.Fatal(err)
	}
	return work
}
