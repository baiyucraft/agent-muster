package work

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestWorkerRepairsReviewsAndPublishesGitCandidateThroughHub(t *testing.T) {
	dsn := os.Getenv("MUSTER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MUSTER_TEST_DATABASE_URL is required")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required")
	}
	t.Setenv("MUSTER_RTC_STUN_URLS", "")
	t.Setenv("MUSTER_RTC_TURN_URLS", "")
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	store, server := workerTestHub(t, ctx, dsn)
	client, err := control.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	worker, identity, dir := workerTestDevice(t, ctx, server, client, "git publisher")
	root, err := worker.resolveDirectory(ctx, dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	remote := t.TempDir()
	git := func(ctx context.Context, dir string, args ...string) (string, error) {
		var output, diagnostic bytes.Buffer
		code, err := runner.RunProjectCommandStreams(ctx, dir, "git", append([]string{"-c", "core.hooksPath=", "-c", "commit.gpgsign=false"}, args...), &output, &diagnostic)
		if err != nil || code != 0 {
			return "", fmt.Errorf("Git %v 退出 %d，错误 %v，诊断 %q", args, code, err, diagnostic.String())
		}
		return strings.TrimSpace(output.String()), nil
	}
	mustGit := func(dir string, args ...string) string {
		t.Helper()
		output, err := git(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	write := func(dir, name, body string) error {
		return os.WriteFile(filepath.Join(dir, name), []byte(body), 0600)
	}
	mustGit(remote, "init", "--bare")
	mustGit(root, "init", "--initial-branch=main")
	mustGit(root, "config", "user.email", "fixture@example.invalid")
	mustGit(root, "config", "user.name", "Muster Fixture")
	mustGit(root, "config", "core.autocrlf", "false")
	mustGit(root, "remote", "add", "origin", remote)
	if err := write(root, "example.txt", "base\n"); err != nil {
		t.Fatal(err)
	}
	mustGit(root, "add", "--", "example.txt")
	mustGit(root, "commit", "-m", "base")
	base := mustGit(root, "rev-parse", "HEAD")
	if err := write(root, "example.txt", "user changes\n"); err != nil {
		t.Fatal(err)
	}
	if err := write(root, "draft.txt", "untracked user file\n"); err != nil {
		t.Fatal(err)
	}
	const projectID = "10000000-0000-4000-8000-000000000031"
	const messageID = "10000000-0000-4000-8000-000000000032"
	const initial = "修复并推送示例"
	const repairNote = "将样例改成 final candidate"
	policy := &gitdelivery.Policy{BaseRef: "refs/heads/main", Remote: "origin", ReviewAgent: "codex", MaxRepairs: 1,
		Test: gitdelivery.Command{Program: "git", Args: []string{"diff", "--exit-code", "HEAD"}, TimeoutSeconds: 20}}
	if _, err := store.SaveProject(ctx, workspace.Project{ID: projectID, Name: "Git fixture", AgentKind: "dsh",
		DeviceID: identity.DeviceID, DirectoryID: dir.ID, GitPolicy: policy}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitMessage(ctx, workspace.Message{ID: messageID, Text: initial}); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.ClaimDelivery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := store.Dispatch(ctx, delivery.ID, workspace.Dispatch{Lease: delivery.Lease, ProjectID: projectID,
		Title: initial, Prompt: initial, GitDelivery: true, GitPublish: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteDelivery(ctx, delivery.ID, workspace.Completion{Lease: delivery.Lease, Status: "handled"}); err != nil {
		t.Fatal(err)
	}
	const nativeSession = "native-git-publication-fixture"
	var fixDir, firstOID string
	var fixes, reviews int
	workerCtx, stop := context.WithCancel(ctx)
	done := startTestWorker(t, workerCtx, worker, identity, client, "",
		func(ctx context.Context, kind, checkout, prompt, session string, env []string, started func() error, announce func(string) error) (string, error) {
			switch kind {
			case "dsh":
				fixes++
				body := "first candidate\n"
				if fixes == 1 {
					if checkout == root || session != "" || !strings.Contains(prompt, initial) {
						return "", fmt.Errorf("修复应从独立工作区和新会话启动")
					}
					fixDir = checkout
				} else if fixes == 2 {
					if checkout != fixDir || session != nativeSession || !strings.Contains(prompt, repairNote) {
						return "", fmt.Errorf("返修须在原工作区续接原会话并带上 Review 反馈")
					}
					body = "final candidate\n"
				} else {
					return "", fmt.Errorf("修复次数超过预算")
				}
				if err := started(); err != nil {
					return "", err
				}
				if err := announce(nativeSession); err != nil {
					return "", err
				}
				if err := write(checkout, "example.txt", body); err != nil {
					return "", err
				}
				_, err := git(ctx, checkout, "commit", "-am", strings.TrimSpace(body))
				return "已修复示例。", err
			case "codex":
				reviews++
				if checkout == fixDir || checkout == root || session != "" || len(env) != 0 {
					return "", fmt.Errorf("独立 Review 应使用新会话和独立检出")
				}
				head, err := git(ctx, checkout, "rev-parse", "HEAD")
				if err != nil {
					return "", err
				}
				if !strings.Contains(prompt, head) || !strings.Contains(prompt, base) || !strings.Contains(prompt, initial) {
					return "", fmt.Errorf("Review 应收到候选提交、基线和验收要求")
				}
				if reviews == 1 {
					firstOID = head
					return "MUSTER_REVIEW_CHANGES\n" + repairNote, nil
				}
				if reviews != 2 || head == firstOID {
					return "", fmt.Errorf("返修后必须审查新的候选提交")
				}
				return "MUSTER_REVIEW_PASS", nil
			default:
				return "", fmt.Errorf("意外的 Agent 类型 %q", kind)
			}
		})
	defer func() { stop(); <-done }()
	detail := awaitWorkerTask(t, ctx, store, taskID, "completed")
	if len(detail.Runs) != 1 || detail.Task.GitPolicyRevision != 1 || !detail.Task.GitPublish {
		t.Fatalf("交付应只占用一个原生任务轮次，并绑定规则版本: %+v", detail.Task)
	}
	run := detail.Runs[0]
	evidence, publication := run.GitVerification, run.GitPublication
	if evidence == nil || !evidence.Valid() || evidence.BaseOID != base || evidence.TestStatus != "passed" ||
		evidence.RepairAttempts != 1 || evidence.ReviewAgent != "codex" || evidence.ReviewStatus != "passed" || run.SessionID != nativeSession {
		t.Fatalf("测试、返修及独立 Review 回执不完整: %+v", evidence)
	}
	ref := "refs/heads/muster/" + taskID
	if publication == nil || publication.Status != "same_commit" || publication.TargetRef != ref ||
		publication.CandidateOID != evidence.CandidateOID || publication.ObservedOID != evidence.CandidateOID || publication.Remote != "origin" {
		t.Fatalf("推送与回读回执不完整: %+v", publication)
	}
	if got := mustGit(remote, "show-ref", "--hash", "--verify", "--", ref); got != evidence.CandidateOID {
		t.Fatalf("裸仓库目标分支指向 %s，预期 %s", got, evidence.CandidateOID)
	}
	if got := mustGit(remote, "show", ref+":example.txt"); got != "final candidate" {
		t.Fatalf("裸仓库保存了意外的候选内容 %q", got)
	}
	logs, err := filepath.Glob(filepath.Join(worker.root, "git", taskID, "test-*", "output.log"))
	if err != nil || len(logs) != 2 {
		t.Fatalf("首次验证和返修应各有测试日志: %v，错误 %v", logs, err)
	}
	for name, want := range map[string]string{"example.txt": "user changes\n", "draft.txt": "untracked user file\n"} {
		if body, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(body) != want {
			t.Fatalf("绑定目录的已有改动 %s 未保留: %q，%v", name, body, err)
		}
	}
	if got := mustGit(root, "rev-parse", "HEAD"); got != base {
		t.Fatalf("原工作区 HEAD 从 %s 变为 %s", base, got)
	}
	report, err := store.ClaimDelivery(ctx)
	if err != nil || report.Kind != "report" || report.TaskID != taskID {
		t.Fatalf("未创建根任务汇报: %+v，%v", report, err)
	}
	for _, want := range []string{ref, evidence.CandidateOID, "Git 测试通过", "独立 Review 通过（codex）"} {
		if !strings.Contains(report.Text, want) {
			t.Fatalf("汇报缺少 %q: %s", want, report.Text)
		}
	}
	completion := workspace.Completion{Lease: report.Lease, Status: "handled", Text: report.Text}
	for range 2 {
		if err := store.CompleteDelivery(ctx, report.ID, completion); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Workspace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reports := 0
	for _, message := range snapshot.Messages {
		if message.Kind == "report" && message.TaskID == taskID {
			reports++
		}
	}
	if reports != 1 {
		t.Fatalf("根任务应只有一条主对话汇报，实际 %d 条", reports)
	}
}
