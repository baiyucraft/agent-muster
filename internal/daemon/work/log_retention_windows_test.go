package work

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRunLogRetentionOnlyRemovesExpiredConfirmedStreams(t *testing.T) {
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	ctx := t.Context()
	now := time.Now()
	cutoff := now.Add(-24 * time.Hour)
	type fileCheck struct {
		path    string
		removed bool
	}
	var checks []fileCheck
	for index, test := range []struct {
		state    string
		sequence int
		pending  string
		expired  bool
		removed  bool
	}{
		{"completed", 2, "", true, true},
		{"failed", 2, "", true, true},
		{"interrupted", 2, "", true, true},
		{"completed", 2, "", false, false},
		{"completed", 2, "receipt", true, false},
		{"completed", 2, "push", true, false},
		{"started", 2, "", true, false},
		{"completed", 0, "", true, false},
		{"", 0, "", true, false},
	} {
		id := fmt.Sprintf("run-%08d-2222-3333-4444-555555555555", index)
		if test.state != "" {
			if _, err := worker.db.ExecContext(ctx, `INSERT INTO runs (id,task_id,directory_id,prompt,session_id,requested_session_id,state,sequence)
				VALUES (?, 'task', 'directory', 'prompt', 'native-session', '', ?, ?)`, id, test.state, test.sequence); err != nil {
				t.Fatal(err)
			}
		}
		switch test.pending {
		case "receipt":
			if _, err := worker.db.ExecContext(ctx, `INSERT INTO outbox VALUES (?, ?, '{}')`, id, test.sequence); err != nil {
				t.Fatal(err)
			}
		case "push":
			if _, err := worker.db.ExecContext(ctx, `INSERT INTO git_push_attempts (run_id,data) VALUES (?, '{}')`, id); err != nil {
				t.Fatal(err)
			}
		}
		mtime := now
		if test.expired {
			mtime = cutoff.Add(-time.Hour)
		}
		invocation := filepath.Join(worker.root, "logs", id, "cli-1")
		checks = append(checks,
			fileCheck{writeRetentionFixture(t, invocation, "stdout.log", mtime), test.removed},
			fileCheck{writeRetentionFixture(t, invocation, "stderr.log", mtime), test.removed},
			fileCheck{writeRetentionFixture(t, invocation, "other.log", mtime), false})
	}
	for range 2 {
		if err := worker.pruneRunLogs(ctx, cutoff); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range checks {
		_, err := os.Stat(check.path)
		if (check.removed && !errors.Is(err, fs.ErrNotExist)) || (!check.removed && err != nil) {
			t.Errorf("文件 %s 的期望删除状态为 %t，Stat 错误为 %v", check.path, check.removed, err)
		}
	}
	var runCount int
	if err := worker.db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE session_id='native-session'`).Scan(&runCount); err != nil || runCount != 8 {
		t.Fatalf("应保留八条运行和会话记录，得到 %d，错误 %v", runCount, err)
	}
}

func TestRunLogRetentionRefusesReparsePointsAndHardLinks(t *testing.T) {
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.db.ExecContext(t.Context(), `INSERT INTO runs (id,task_id,directory_id,prompt,session_id,requested_session_id,state,sequence)
		VALUES (?, 'task', 'directory', '', '', '', 'completed', 2)`, logFixtureRunID); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(worker.root, "logs", logFixtureRunID)
	invocation := filepath.Join(run, "cli-1")
	if err := os.MkdirAll(invocation, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(worker.root, "outside")
	cutoff := time.Now().Add(-24 * time.Hour)
	original := writeRetentionFixture(t, outside, "stdout.log", cutoff.Add(-time.Hour))
	hardLink := filepath.Join(invocation, "stdout.log")
	if err := os.Link(original, hardLink); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "cmd.exe", "/d", "/c", "mklink", "/J", "cli-2", `..\..\outside`)
	cmd.Dir = run
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("创建 Windows 目录联接失败：%v\n%s", err, output)
	}
	if err := worker.pruneRunLogs(t.Context(), cutoff); err == nil {
		t.Fatal("清理必须报告跳过不安全的日志路径")
	}
	for _, name := range []string{original, hardLink, filepath.Join(run, "cli-2", "stdout.log")} {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("不安全路径必须保留，Stat 错误为 %v", err)
		}
	}
}

func writeRetentionFixture(t *testing.T, dir, name string, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("Muster log fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}
