package work

import (
	"context"
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
	git(root, nil, "add", "example.txt")
	git(root, nil, "commit", "-m", "base")
	write(root, "example.txt", "unrelated local changes")
	const projectID = "10000000-0000-4000-8000-000000000021"
	policy := &gitdelivery.Policy{BaseRef: "refs/heads/main", Remote: "origin",
		Test: gitdelivery.Command{Program: "git", Args: []string{"diff", "--exit-code", "HEAD"}, TimeoutSeconds: 20}}
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
	workerCtx, stop := context.WithCancel(ctx)
	done := startTestWorker(t, workerCtx, worker, identity, client, "",
		func(_ context.Context, _, checkout, _, _ string, _ []string, started func() error, announce func(string) error) (string, error) {
			if err := started(); err != nil {
				return "", err
			}
			if err := announce("native-git-fixture"); err != nil {
				return "", err
			}
			write(checkout, "example.txt", "candidate")
			git(checkout, nil, "add", "example.txt")
			git(checkout, nil, "commit", "-m", "candidate")
			return "样例已修复", nil
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
}
