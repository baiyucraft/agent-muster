package work

import (
	"context"
	"errors"
	"io"
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

func TestWorkerVerifiesGitCandidateThroughHub(t *testing.T) {
	dsn := os.Getenv("MUSTER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MUSTER_TEST_DATABASE_URL is required")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required")
	}
	t.Setenv("MUSTER_RTC_STUN_URLS", "")
	t.Setenv("MUSTER_RTC_TURN_URLS", "")
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	store, server := workerTestHub(t, ctx, dsn)
	client, err := control.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	worker, identity, dir := workerTestDevice(t, ctx, server, client, "git verifier")
	root, err := worker.resolveDirectory(ctx, dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	git := func(dir string, output io.Writer, args ...string) {
		t.Helper()
		if code, err := runner.RunProjectCommand(ctx, dir, "git", args, output); err != nil || code != 0 {
			t.Fatalf("Git %v returned %d: %v", args, code, err)
		}
	}
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(root, nil, "init", "--initial-branch=main")
	git(root, nil, "config", "user.email", "fixture@example.invalid")
	git(root, nil, "config", "user.name", "Muster Fixture")
	write(root, "example.txt", "base")
	write(root, "required.txt", "required")
	git(root, nil, "add", "example.txt", "required.txt")
	git(root, nil, "commit", "-m", "base")
	write(root, "example.txt", "unrelated local changes")
	const projectID = "10000000-0000-4000-8000-000000000021"
	policy := &gitdelivery.Policy{BaseRef: "refs/heads/main", Remote: "origin", MaxRepairs: 1,
		Test: gitdelivery.Command{Program: "git", Args: []string{"cat-file", "-e", "HEAD:required.txt"}, TimeoutSeconds: 20}}
	if _, err := store.SaveProject(ctx, workspace.Project{ID: projectID, Name: "Git fixture",
		DeviceID: identity.DeviceID, DirectoryID: dir.ID, GitPolicy: policy}); err != nil {
		t.Fatal(err)
	}
	const requestID = "10000000-0000-4000-8000-000000000022"
	if _, err := store.SubmitMessage(ctx, workspace.Message{ID: requestID, Text: "验证候选提交"}); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.ClaimDelivery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := store.Dispatch(ctx, delivery.ID, workspace.Dispatch{
		Lease: delivery.Lease, ProjectID: projectID, Title: "Git fixture", Prompt: "Fix the sample", GitDelivery: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteDelivery(ctx, delivery.ID, workspace.Completion{Lease: delivery.Lease, Status: "handled"}); err != nil {
		t.Fatal(err)
	}
	const nativeSession = "native-git-fixture"
	const followupID = "10000000-0000-4000-8000-000000000023"
	const followupText = "在原候选中补充说明"
	inputID := "message:" + followupID
	workerCtx, stop := context.WithCancel(ctx)
	done := startTestWorker(t, workerCtx, worker, identity, client, "",
		func(_ context.Context, _, checkout, prompt, session string, env []string, started func() error, announce func(string) error) (string, error) {
			if err := started(); err != nil {
				return "", err
			}
			if err := announce(nativeSession); err != nil {
				return "", err
			}
			switch {
			case session == "":
				write(checkout, "example.txt", "candidate")
				git(checkout, nil, "add", "example.txt")
				git(checkout, nil, "commit", "-m", "candidate")
				return "样例已修复", nil
			case session == nativeSession && strings.HasPrefix(prompt, "上一回复的结构化回执格式无效"):
				if len(env) != 0 || !strings.Contains(prompt, inputID) {
					return "", errors.New("补写回执须沿用原会话且只包含本轮 ID，不应授予协作权限")
				}
				return strings.TrimSpace(testWorkerReply("", inputID)), nil
			case session == nativeSession && strings.HasPrefix(prompt, "验证发现以下问题"):
				if strings.Contains(prompt, inputReceiptMarker) {
					return "", errors.New("返修不应要求再确认已处理输入")
				}
				write(checkout, "required.txt", "required")
				git(checkout, nil, "add", "required.txt")
				git(checkout, nil, "commit", "-m", "restore required file")
				return "追加说明及返修完成。", nil
			case session == nativeSession && strings.Contains(prompt, followupText) && strings.Contains(prompt, inputID):
				write(checkout, "example.txt", "candidate with notes")
				git(checkout, nil, "add", "example.txt")
				git(checkout, nil, "rm", "required.txt")
				git(checkout, nil, "commit", "-m", "follow-up candidate")
				return "追加说明完成。", nil
			default:
				return "", errors.New("收到了意外的 Git 会话输入")
			}
		})
	defer func() { stop(); <-done }()
	detail := awaitWorkerTask(t, ctx, store, taskID, "completed")
	if detail.Task.GitPolicyRevision != 1 || len(detail.Runs) != 1 {
		t.Fatalf("Git 规则快照或运行数量不一致：%+v", detail)
	}
	evidence := detail.Runs[0].GitVerification
	if evidence == nil || !evidence.Valid() || evidence.TestStatus != "passed" || !strings.Contains(detail.Task.Result, evidence.CandidateOID) {
		t.Fatalf("Git 候选证据无效：%+v", evidence)
	}
	if body, err := os.ReadFile(filepath.Join(root, "example.txt")); err != nil || string(body) != "unrelated local changes" {
		t.Fatalf("绑定目录中的已有改动未保留：%q，%v", body, err)
	}
	if _, err := store.SubmitMessage(ctx, workspace.Message{ID: followupID, TaskID: taskID, Text: followupText}); err != nil {
		t.Fatal(err)
	}
	followup, err := store.ClaimDelivery(ctx)
	if err != nil || followup.MessageID != followupID {
		t.Fatalf("待派发追加应先于先前任务汇报：%+v，%v", followup, err)
	}
	if _, err := store.Dispatch(ctx, followup.ID, workspace.Dispatch{Lease: followup.Lease, TaskID: taskID, Prompt: followupText}); err != nil {
		t.Fatal(err)
	}
	repaired := awaitWorkerTask(t, ctx, store, taskID, "completed")
	if len(repaired.Runs) != 2 || repaired.Runs[1].SessionID != nativeSession || !strings.Contains(repaired.Task.Result, "追加说明及返修完成。") {
		t.Fatalf("补写和返修应在一个续接轮次中完成：%+v", repaired)
	}
	if evidence := repaired.Runs[1].GitVerification; evidence == nil || evidence.RepairAttempts != 1 || evidence.TestStatus != "passed" {
		t.Fatalf("缺少返修后的 Git 验证：%+v", evidence)
	}
	for _, input := range repaired.Inputs {
		if input.MessageID == followupID && input.ReceiptStatus == "handled" && input.ReceiptRunID == repaired.Runs[1].ID {
			return
		}
	}
	t.Fatalf("返修后未保留原输入回执：%+v", repaired.Inputs)
}
