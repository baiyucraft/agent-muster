package work

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/runreport"
)

const logFixtureRunID = "run-11111111-2222-3333-4444-555555555555"

func TestRunLogsReadsRecentInvocationTails(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "logs", logFixtureRunID, "cli-999")
	newer := filepath.Join(root, "logs", logFixtureRunID, "cli-111")
	for _, dir := range []string{old, newer} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const suffix = "结尾"
	text := strings.Repeat("日志", runreport.MaxLogTextBytes) + "\xff" + suffix
	if err := os.WriteFile(filepath.Join(newer, "stdout.log"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "stderr.log"), []byte("earlier"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(old, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	logs, err := readLocalRunLogs(root, logFixtureRunID, "")
	if err != nil || logs.Validate() != nil || len(logs.Files) != 2 {
		t.Fatalf("本机日志状态为 %q，片段 %d，错误 %v", logs.Status, len(logs.Files), err)
	}
	file := logs.Files[0]
	if file.Invocation != "cli-111" || file.Stream != "stdout" || !file.Truncated || file.UpdatedAt == nil || !strings.HasSuffix(file.Text, "�"+suffix) {
		t.Fatalf("最新片段的标识、时间或截断状态无效：%s %s %t %v", file.Invocation, file.Stream, file.Truncated, file.UpdatedAt)
	}
}

func TestRunLogsSelectsExactReviewInvocation(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"review-1": "first review",
		"review-2": "second review",
		"cli-1":    "repair output",
	} {
		dir := filepath.Join(root, "logs", logFixtureRunID, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stdout.log"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := readLocalRunLogs(root, logFixtureRunID, "review-2")
	if err != nil || logs.Validate() != nil || len(logs.Files) != 1 ||
		logs.Files[0].Invocation != "review-2" || logs.Files[0].Text != "second review" {
		t.Fatalf("精确 Review 日志查询读到了其他调用：%+v，错误 %v", logs, err)
	}
}

func TestRunLogsRefusesLinksAndUnknownRun(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	worker, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	const logRequestID = "log-1234567890abcdef1234567890abcdef"
	request := runreport.LogRequest{ID: logRequestID, RunID: logFixtureRunID}
	dir := filepath.Join(root, "logs", request.RunID, "cli-1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "fixture.txt")
	if err := os.WriteFile(fixture, []byte("fixture outside the requested run"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := worker.loadRunLogs(ctx, request); got.Status != runreport.LogMissing || len(got.Files) != 0 {
		t.Fatalf("未登记运行返回日志：%+v", got)
	}
	if _, err := worker.db.ExecContext(ctx, `INSERT INTO runs (id,task_id,directory_id,prompt,session_id,requested_session_id,state) VALUES (?, 'task', 'directory', '', '', '', 'started')`, request.RunID); err != nil {
		t.Fatal(err)
	}
	if got := worker.loadRunLogs(ctx, request); got.Status != runreport.LogMissing || len(got.Files) != 0 {
		t.Fatalf("活动运行返回日志：%+v", got)
	}
	if _, err := worker.db.ExecContext(ctx, `UPDATE runs SET state='completed' WHERE id=?`, request.RunID); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(fixture, filepath.Join(dir, "stdout.log")); err != nil {
		t.Fatal(err)
	}
	if got := worker.loadRunLogs(ctx, request); got.Status != runreport.LogFailed || len(got.Files) != 0 {
		t.Fatalf("硬链接不得返回日志：%+v", got)
	}
}

func TestRunLogsRefusesReparsePointInInvocationPath(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "logs", logFixtureRunID)
	if err := os.MkdirAll(run, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "stdout.log"), []byte("outside requested run"), 0600); err != nil {
		t.Fatal(err)
	}
	// 普通 Windows 用户可以建立目录联接，无需符号链接权限。
	cmd := exec.CommandContext(t.Context(), "cmd.exe", "/d", "/c", "mklink", "/J", "cli-1", `..\..\outside`)
	cmd.Dir = run
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("创建 Windows 目录联接失败：%v\n%s", err, output)
	}
	logs, err := readLocalRunLogs(root, logFixtureRunID, "")
	if err == nil || len(logs.Files) != 0 {
		t.Fatal("重解析点不得返回日志")
	}
}
