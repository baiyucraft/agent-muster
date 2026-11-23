package work

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
)

func TestApprovalReply(t *testing.T) {
	for _, test := range []struct {
		name, text string
		approval   bool
		malformed  bool
	}{
		{name: "plain text", text: "See the format " + approvalMarker},
		{name: "valid", text: approvalMarker + "\r\n" + `{"question":" CSV 还是 JSON？ ","options":["CSV","JSON"]}`, approval: true},
		{name: "fenced", text: "```text\n" + approvalMarker + "\n{}\n```", malformed: true},
		{name: "unknown field", text: approvalMarker + "\n" + `{"question":"选择？","unexpected":true}`, malformed: true},
		{name: "duplicate options", text: approvalMarker + "\n" + `{"question":"选择？","options":["CSV"," CSV "]}`, malformed: true},
		{name: "trailing content", text: approvalMarker + "\n" + `{"question":"选择？"} done`, malformed: true},
		{name: "missing body", text: approvalMarker, malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			question, err := parseApprovalReply(test.text)
			if (err != nil) != test.malformed || (question != nil) != test.approval {
				t.Fatalf("question=%+v err=%v", question, err)
			}
			if question != nil && (question.Question != "CSV 还是 JSON？" || len(question.Options) != 2) {
				t.Fatalf("unexpected normalized question: %+v", question)
			}
		})
	}
}

func TestWorkerApprovalReceipt(t *testing.T) {
	for _, test := range []struct {
		name              string
		supported         bool
		child             bool
		initial, recovery string
		kind              string
	}{
		{name: "valid", supported: true, initial: approvalMarker + "\n" + `{"question":"继续吗？"}`, kind: "approval_requested"},
		{name: "old hub", initial: approvalMarker + "\n" + `{"question":"继续吗？"}`, kind: "completed"},
		{name: "reformatted", supported: true, initial: approvalMarker + "\n{", recovery: approvalMarker + "\n" + `{"question":"继续吗？"}`, kind: "approval_requested"},
		{name: "invalid after retry", supported: true, initial: approvalMarker + "\n{", recovery: "ordinary text", kind: "interrupted"},
		{name: "invalid with NUL", supported: true, initial: approvalMarker + "\n\x00", recovery: "ordinary text", kind: "interrupted"},
		{name: "invalid child", supported: true, child: true, initial: approvalMarker + "\n{", recovery: "ordinary text", kind: "interrupted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer worker.Close()
			ctx := context.Background()
			directory, err := worker.RegisterDirectory(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			run := control.Run{ID: "run-1", TaskID: "task-1", DirectoryID: directory.ID, Prompt: "prepare index", ApprovalSupported: test.supported}
			if test.child {
				run.ParentDeviceID = "parent-device"
			}
			if err := worker.saveClaim(ctx, run); err != nil {
				t.Fatal(err)
			}
			var calls int
			execute := func(_ context.Context, _, _, prompt, sessionID string, _ []string, started func() error, session func(string) error) (string, error) {
				calls++
				if err := started(); err != nil {
					return "", err
				}
				if err := session("session-1"); err != nil {
					return "", err
				}
				if calls == 1 && (strings.Contains(prompt, approvalMarker) == test.supported) && sessionID == "" {
					return test.initial, nil
				}
				if calls == 2 && test.recovery != "" && sessionID == "session-1" && strings.Contains(prompt, "只重新输出") {
					return test.recovery, nil
				}
				return "", errors.New("unexpected execution")
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
			if event.Kind != test.kind || event.SessionID != "session-1" {
				t.Fatalf("unexpected terminal event: %+v", event)
			}
			if test.kind == "approval_requested" && (event.Approval == nil || event.Text != "" || event.Approval.Options == nil) {
				t.Fatalf("question not preserved in outbox: %+v", event)
			}
			if test.recovery != "" && calls != 2 || test.recovery == "" && calls != 1 {
				t.Fatalf("unexpected number of invocations: %d", calls)
			}
			if test.kind == "interrupted" {
				var original string
				if err := worker.db.QueryRowContext(ctx, `SELECT text FROM runs WHERE id=?`, run.ID).Scan(&original); err != nil {
					t.Fatal(err)
				}
				if original != test.initial || strings.Contains(event.Text, approvalMarker) || strings.ContainsRune(event.Text, 0) {
					t.Fatalf("original reply and reported diagnostic were not separated: %q, %q", original, event.Text)
				}
				if test.child && event.Text != "" {
					t.Fatal("child reply was sent through Hub")
				}
			}
		})
	}
}
