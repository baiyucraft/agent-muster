//go:build windows

package gitwork

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
)

func TestPrepareFreezesExactBaseAndPreservesOriginalWorktree(t *testing.T) {
	root, task, base := fixtureRepo(t)
	policy := fixturePolicy()
	// 创建一个与完整引用同名的标签，验证基线查找采用完整的字面引用。
	localGit(t, root, "commit", "--allow-empty", "-m", "unrelated to baseline ref")
	localGit(t, root, "tag", "refs/heads/delivery-base")
	writeFixture(t, filepath.Join(root, "data.txt"), "user change")
	writeFixture(t, filepath.Join(root, "untracked.txt"), "user draft")

	work, err := Prepare(t.Context(), root, task, policy)
	if err != nil {
		t.Fatal(err)
	}
	if work.BaseOID != base || localGit(t, work.FixDir, "rev-parse", "HEAD") != base {
		t.Fatalf("修复工作区未使用显式基线 %s: %+v", base, work)
	}
	localGit(t, root, "branch", "-f", "delivery-base", "HEAD")
	writeFixture(t, filepath.Join(work.FixDir, "data.txt"), "fix in progress")
	recovered, err := Prepare(t.Context(), root, task, policy)
	if err != nil || recovered.BaseOID != base || recovered.FixDir != work.FixDir {
		t.Fatalf("恢复改变了基线或修复目录: %+v, %v", recovered, err)
	}
	assertFixture(t, filepath.Join(root, "data.txt"), "user change")
	assertFixture(t, filepath.Join(root, "untracked.txt"), "user draft")
	assertFixture(t, filepath.Join(work.FixDir, "data.txt"), "fix in progress")
	if _, err := work.Candidate(t.Context()); err == nil {
		t.Fatal("不能把未提交的修复作为待测候选")
	}
	localGit(t, work.FixDir, "commit", "-am", "fix commit")
	want := localGit(t, work.FixDir, "rev-parse", "HEAD")
	if got, err := work.Candidate(t.Context()); err != nil || got != want {
		t.Fatalf("读取候选: %q, %v, 预期 %q", got, err, want)
	}
}

func TestPrepareRejectsOverlappingAndNestedBindings(t *testing.T) {
	root, task, _ := fixtureRepo(t)
	nested := filepath.Join(root, "sub")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range [][2]string{{root, filepath.Join(nested, "task")}, {nested, task}, {root, filepath.Dir(root)}} {
		if _, err := Prepare(t.Context(), tc[0], tc[1], fixturePolicy()); err == nil {
			t.Fatalf("接受重叠或非根绑定: %q", tc)
		}
	}
}

func TestCandidateRejectsUntrackedFilesAndUnrelatedCommits(t *testing.T) {
	root, task, base := fixtureRepo(t)
	work, candidate := fixtureCandidate(t, root, task, fixturePolicy())
	if candidate == base {
		t.Fatal("测试必须产生不同的候选")
	}
	draft := filepath.Join(work.FixDir, "new.txt")
	writeFixture(t, draft, "not staged")
	if _, err := work.Candidate(t.Context()); err == nil {
		t.Fatal("候选校验接受未跟踪文件")
	}
	if err := os.Remove(draft); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(work.FixDir, ".muster-inbox")
	if err := os.Mkdir(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(inbox, "input.txt"), "private attachment")
	if got, err := work.Candidate(t.Context()); err != nil || got != candidate {
		t.Fatalf("设备收件阻止了候选验证：%q，%v", got, err)
	}
	localGit(t, work.FixDir, "add", ".muster-inbox/input.txt")
	localGit(t, work.FixDir, "commit", "-m", "add inbox")
	localGit(t, work.FixDir, "rm", ".muster-inbox/input.txt")
	localGit(t, work.FixDir, "commit", "-m", "remove inbox")
	if _, err := work.Candidate(t.Context()); err == nil {
		t.Fatal("候选提交历史包含设备收件内容")
	}
	localGit(t, work.FixDir, "switch", "--orphan", "unrelated")
	localGit(t, work.FixDir, "commit", "--allow-empty", "-m", "unrelated")
	localGit(t, work.FixDir, "checkout", "--detach", "HEAD")
	if _, err := work.Candidate(t.Context()); err == nil {
		t.Fatal("候选校验接受无共同基线的提交")
	}
}

func TestTestBindsEvidenceToCleanCandidate(t *testing.T) {
	root, task, base := fixtureRepo(t)
	policy := processFixturePolicy(t, 30)
	work, candidate := fixtureCandidate(t, root, task, policy)
	if _, err := work.Test(t.Context(), base, policy.Test); err == nil {
		t.Fatal("不能为旧提交记录候选测试证据")
	}
	t.Setenv("GITWORK_TEST_FIXTURE", "pass")
	result, err := work.Test(t.Context(), candidate, policy.Test)
	if err != nil || result.CandidateOID != candidate || result.ExitCode == nil || *result.ExitCode != 0 || result.LogRef == "" || !result.LogTruncated {
		t.Fatalf("缺少成功测试或上限证据: %+v, %v", result, err)
	}
	logPath := filepath.Join(task, filepath.FromSlash(result.LogRef))
	log, err := os.ReadFile(logPath)
	if err != nil || len(log) != maxTestLog || !strings.Contains(string(log), "candidate output") {
		t.Fatalf("日志长度 %d，错误 %v", len(log), err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(logPath), "checkout")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("测试成功后应释放独立工作区: %v", err)
	}
	t.Setenv("GITWORK_TEST_FIXTURE", "fail")
	failed, err := work.Test(t.Context(), candidate, policy.Test)
	if err != nil || failed.ExitCode == nil || *failed.ExitCode != 7 || failed.LogRef == result.LogRef {
		t.Fatalf("测试失败应保留退出码与独立日志: %+v, %v", failed, err)
	}
	t.Setenv("GITWORK_TEST_FIXTURE", "dirty")
	if dirty, err := work.Test(t.Context(), candidate, policy.Test); err == nil || dirty.ExitCode == nil || *dirty.ExitCode != 0 {
		t.Fatalf("必须识别退出码为零的受测文件修改: %+v, %v", dirty, err)
	}
	assertFixture(t, filepath.Join(work.FixDir, "data.txt"), "candidate\n")
	assertFixture(t, filepath.Join(root, "data.txt"), "base\n")
}

func TestTestTimeoutPreservesAttemptAndAllowsFreshCheckout(t *testing.T) {
	root, task, _ := fixtureRepo(t)
	policy := processFixturePolicy(t, 2)
	work, candidate := fixtureCandidate(t, root, task, policy)
	t.Setenv("GITWORK_TEST_FIXTURE", "timeout")
	interrupted, err := work.Test(t.Context(), candidate, policy.Test)
	if !errors.Is(err, context.DeadlineExceeded) || interrupted.ExitCode != nil || interrupted.LogRef == "" {
		t.Fatalf("测试超时后应保留未知退出码及本机证据: %+v, %v", interrupted, err)
	}
	firstAttempt := filepath.Dir(filepath.Join(task, filepath.FromSlash(interrupted.LogRef)))
	assertFixture(t, filepath.Join(firstAttempt, "checkout", "data.txt"), "candidate\n")
	t.Setenv("GITWORK_TEST_FIXTURE", "pass")
	retry, err := work.Test(t.Context(), candidate, policy.Test)
	if err != nil || retry.ExitCode == nil || *retry.ExitCode != 0 || retry.LogRef == interrupted.LogRef {
		t.Fatalf("重新测试应使用新的独立工作区: %+v, %v", retry, err)
	}
}

func TestGitWorkCommandFixture(t *testing.T) {
	mode := os.Getenv("GITWORK_TEST_FIXTURE")
	if mode == "" {
		return
	}
	assertFixture(t, "data.txt", "candidate\n")
	switch mode {
	case "pass":
		_, _ = os.Stdout.WriteString(strings.Repeat("candidate output\n", maxTestLog/10))
		writeFixture(t, "untracked-test-output.txt", "build output")
	case "fail":
		_, _ = os.Stderr.WriteString("project test failed")
		os.Exit(7)
	case "dirty":
		writeFixture(t, "data.txt", "changed during test")
	case "timeout":
		_, _ = os.Stdout.WriteString("test started")
		time.Sleep(15 * time.Second)
	}
}

func processFixturePolicy(t *testing.T, timeout int) gitdelivery.Policy {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(executable)+string(os.PathListSeparator)+os.Getenv("PATH"))
	policy := fixturePolicy()
	policy.Test = gitdelivery.Command{Program: filepath.Base(executable), Args: []string{"-test.run=^TestGitWorkCommandFixture$"}, TimeoutSeconds: timeout}
	return policy
}

func fixtureRepo(t *testing.T) (root, task, base string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git 不可用")
	}
	root, task = t.TempDir(), t.TempDir()
	localGit(t, root, "init", "--initial-branch=main")
	localGit(t, root, "config", "user.name", "Muster Git Fixture")
	localGit(t, root, "config", "user.email", "fixture@example.invalid")
	localGit(t, root, "config", "commit.gpgsign", "false")
	localGit(t, root, "config", "core.autocrlf", "false")
	writeFixture(t, filepath.Join(root, "data.txt"), "base\n")
	localGit(t, root, "add", "--", "data.txt")
	localGit(t, root, "commit", "-m", "baseline")
	localGit(t, root, "branch", "delivery-base")
	return root, task, localGit(t, root, "rev-parse", "HEAD")
}

func fixturePolicy() gitdelivery.Policy {
	return gitdelivery.Policy{BaseRef: "refs/heads/delivery-base", Remote: "origin", Test: gitdelivery.Command{Program: "git", TimeoutSeconds: 30}}
}

func fixtureCandidate(t *testing.T, root, task string, policy gitdelivery.Policy) (Workspace, string) {
	t.Helper()
	work, err := Prepare(t.Context(), root, task, policy)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(work.FixDir, "data.txt"), "candidate\n")
	localGit(t, work.FixDir, "commit", "-am", "candidate")
	candidate, err := work.Candidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return work, candidate
}

func localGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(context.Background(), "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("git %v: %v\n%s", args, err, exit.Stderr)
		}
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertFixture(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("读取 %s: %q, %v, 预期 %q", path, data, err, expected)
	}
}
