package work

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

func TestWorkerReportsConfirmedSessionFailure(t *testing.T) {
	for _, test := range []struct {
		name, kind, session, evidence, returnedText, want string
		failure                                           error
		supported, announced                              bool
	}{
		{name: "会话占用", kind: "dsh", session: "session-1", failure: runner.ErrSessionBusy, supported: true, want: "busy"},
		{name: "会话不可用", kind: "dsh", session: "session-1", failure: runner.ErrSessionUnavailable, supported: true, want: "unavailable"},
		{name: "旧 Hub", kind: "dsh", session: "session-1", failure: runner.ErrSessionBusy},
		{name: "Codex 不识别占用", kind: "codex", session: "session-1", failure: runner.ErrSessionBusy, supported: true},
		{name: "Codex 会话不可用", kind: "codex", session: "session-1", failure: runner.ErrSessionUnavailable, supported: true, want: "unavailable"},
		{name: "Codex 会话已打开", kind: "codex", session: "session-1", failure: runner.ErrSessionUnavailable, supported: true, announced: true},
		{name: "首次执行", kind: "dsh", failure: runner.ErrSessionBusy, supported: true},
		{name: "普通进程失败", kind: "dsh", session: "session-1", failure: errors.New("process closed"), supported: true},
		{name: "已打开会话", kind: "dsh", session: "session-1", failure: runner.ErrSessionBusy, supported: true, announced: true},
		{name: "已收到文本", kind: "dsh", session: "session-1", failure: runner.ErrSessionBusy, supported: true, returnedText: "工作结果"},
		{name: "已存等待记录", kind: "dsh", session: "session-1", failure: runner.ErrSessionBusy, supported: true,
			evidence: `INSERT INTO helper_waits(run_id,payload) VALUES (?, '{}')`},
		{name: "已存输入回执原文", kind: "dsh", session: "session-1", failure: runner.ErrSessionBusy, supported: true,
			evidence: `INSERT INTO input_receipt_original_replies(run_id,body) VALUES (?, '确认输入')`},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			worker, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer worker.Close()
			directory, err := worker.RegisterDirectory(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			run := control.Run{
				ID: "run-1", TaskID: "task-1", DirectoryID: directory.ID, Prompt: "完成任务",
				AgentKind: test.kind, SessionID: test.session, SessionFailureSupported: test.supported,
			}
			if err := worker.saveClaim(ctx, run); err != nil {
				t.Fatal(err)
			}
			execute := func(_ context.Context, _, _, _, sessionID string, _ []string, started func() error, session func(string) error) (string, error) {
				if err := started(); err != nil {
					return "", err
				}
				if test.announced {
					if err := session(sessionID); err != nil {
						return "", err
					}
				}
				if test.evidence != "" {
					if _, err := worker.db.ExecContext(ctx, test.evidence, run.ID); err != nil {
						return "", err
					}
				}
				return test.returnedText, test.failure
			}
			if err := worker.execute(ctx, run, nil, nil, execute); err != nil {
				t.Fatal(err)
			}
			var payload string
			if err := worker.db.QueryRowContext(ctx, `SELECT payload FROM outbox WHERE run_id=? ORDER BY sequence DESC LIMIT 1`, run.ID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var event control.RunEvent
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				t.Fatal(err)
			}
			sessionID := test.session
			if test.want != "" {
				sessionID = ""
			}
			if event.Kind != "failed" || event.SessionFailure != test.want || event.SessionID != sessionID {
				t.Fatalf("终态 = %+v", event)
			}
			var storedID string
			if err := worker.db.QueryRowContext(ctx, `SELECT session_id FROM runs WHERE id=?`, run.ID).Scan(&storedID); err != nil || storedID != test.session {
				t.Fatalf("本机原会话 = %q，错误 %v", storedID, err)
			}
		})
	}
}
