package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

const (
	inputTestMessage = "message:message-1"
	inputTestResult  = "transfer:transfer-1"
	inputTestAll     = inputReceiptMarker + "\n" + `{"handled_input_ids":["` + inputTestMessage + `","` + inputTestResult + `"]}`
	inputTestPartial = inputReceiptMarker + "\n" + `{"handled_input_ids":["` + inputTestResult + `"]}`
	inputTestEmpty   = inputReceiptMarker + "\n" + `{"handled_input_ids":[]}`
)

func TestParseInputReceipt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		footer string
		valid  bool
	}{
		{name: "完整", footer: inputTestAll, valid: true},
		{name: "部分及 CRLF", footer: strings.ReplaceAll(inputTestPartial, "\n", "\r\n") + "\r\n", valid: true},
		{name: "明确空数组", footer: inputTestEmpty, valid: true},
		{name: "外部 ID", footer: inputReceiptMarker + "\n" + `{"handled_input_ids":["message:other"]}`},
		{name: "重复 ID", footer: inputReceiptMarker + "\n" + `{"handled_input_ids":["` + inputTestMessage + `","` + inputTestMessage + `"]}`},
		{name: "重复键", footer: inputReceiptMarker + "\n" + `{"handled_input_ids":[],"handled_input_ids":[]}`},
		{name: "未知键", footer: inputReceiptMarker + "\n" + `{"handled_input_ids":[],"extra":true}`},
		{name: "空值", footer: inputReceiptMarker + "\n" + `{"handled_input_ids":null}`},
		{name: "多行", footer: inputReceiptMarker + "\n" + "{\n\"handled_input_ids\":[]}"},
		{name: "末尾多余内容", footer: inputTestAll + "\nextra"},
		{name: "围栏", footer: "```text\n" + inputTestAll + "\n```"},
		{name: "缺少标记", footer: `{"handled_input_ids":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, receipt := parseInputReceipt("产物已保存。\n\n"+tc.footer, []string{inputTestMessage, inputTestResult})
			if (receipt != nil) != tc.valid || tc.valid && body != "产物已保存。" {
				t.Fatalf("正文 %q，回执 %+v", body, receipt)
			}
		})
	}
}

func TestWorkerInputReceipt(t *testing.T) {
	const original = "已处理，文件已保存。"
	const question = approvalMarker + "\n" + `{"question":"请选择文件格式"}`
	const broken = inputReceiptMarker + "\n{"
	for _, tc := range []struct {
		name    string
		initial string
		repair  string
		kind    string
		handled []string
		wait    bool
		noInput bool
		persist bool
		failure error
		cancel  bool
		pending bool
	}{
		{name: "普通兼容", initial: original, kind: "completed", noInput: true},
		{name: "完整", initial: original + "\n" + inputTestAll, kind: "completed", handled: []string{inputTestMessage, inputTestResult}},
		{name: "补写不覆盖原结果", initial: original + "\n" + broken, repair: inputTestAll, kind: "completed", handled: []string{inputTestMessage, inputTestResult}, persist: true},
		{name: "不发布损坏的多行回执", initial: original + "\n" + inputTestAll + "\n或者其他字段", repair: inputTestAll, kind: "completed", handled: []string{inputTestMessage, inputTestResult}, persist: true},
		{name: "围栏格式补写", initial: original + "\n```text\n" + inputTestAll + "\n```", repair: inputTestAll, kind: "completed", handled: []string{inputTestMessage, inputTestResult}, persist: true},
		{name: "补写仍缺失", initial: original, repair: "还没做完", kind: "interrupted", persist: true},
		{name: "不接受补写新的业务结果", initial: original, repair: "新的结果\n" + inputTestAll, kind: "interrupted", persist: true},
		{name: "补写有效的部分回执", initial: original + "\n" + inputTestPartial, repair: inputTestAll, kind: "completed", handled: []string{inputTestMessage, inputTestResult}, persist: true},
		{name: "补写后仍未确认全部输入", initial: original + "\n" + inputTestPartial, repair: inputTestPartial, kind: "interrupted", handled: []string{inputTestResult}, persist: true},
		{name: "补写无效保留原部分回执", initial: original + "\n" + inputTestPartial, repair: "格式仍不正确", kind: "interrupted", handled: []string{inputTestResult}, persist: true},
		{name: "补写不能撤销原部分回执", initial: original + "\n" + inputTestPartial, repair: inputTestEmpty, kind: "interrupted", handled: []string{inputTestResult}, persist: true},
		{name: "空正文不能完成", initial: inputTestAll, kind: "interrupted", handled: []string{inputTestMessage, inputTestResult}, persist: true},
		{name: "非法正文不能补写业务结果", initial: "\x00\n" + inputTestAll, kind: "interrupted", handled: []string{inputTestMessage, inputTestResult}, persist: true},
		{name: "请示可部分确认", initial: question + "\n" + inputTestPartial, kind: "approval_requested", handled: []string{inputTestResult}},
		{name: "等待可以空确认", initial: inputTestEmpty, kind: "waiting", wait: true, handled: []string{}},
		{name: "请示与等待相冲突", initial: question + "\n" + inputTestPartial, kind: "interrupted", wait: true, handled: []string{inputTestResult}, persist: true},
		{name: "同时补写两个协议", initial: approvalMarker + "\n{\n" + broken, repair: question + "\n" + inputTestPartial, kind: "approval_requested", handled: []string{inputTestResult}, persist: true},
		{name: "只补写请示", initial: approvalMarker + "\n{\n" + inputTestPartial, repair: question, kind: "approval_requested", handled: []string{inputTestResult}, persist: true},
		{name: "只补写回执", initial: question + "\n" + broken, repair: inputTestPartial, kind: "approval_requested", handled: []string{inputTestResult}, persist: true},
		{name: "CLI 失败时仍提交已处理输入", initial: original + "\n" + inputTestPartial, kind: "failed", handled: []string{inputTestResult}, failure: errors.New("CLI failed")},
		{name: "CLI 失败时仅接受有效回执", initial: original + "\n" + broken, kind: "failed", failure: errors.New("CLI failed")},
		{name: "进程退出后中断仍保留部分回执", initial: original + "\n" + inputTestPartial, kind: "interrupted", handled: []string{inputTestResult}, failure: context.Canceled, cancel: true},
		{name: "进程退出未确认时不提交终态或回执", initial: original + "\n" + inputTestPartial, failure: runner.ErrProcessExitUnconfirmed, pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer worker.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			binding, err := worker.RegisterDirectory(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			run := control.Run{ID: "run-1", TaskID: "task-1", DirectoryID: binding.ID, Prompt: "完成工作", ApprovalSupported: true}
			if !tc.noInput {
				run.InputIDs = []string{inputTestMessage, inputTestResult}
				run.InputMessages = map[string]string{inputTestMessage: "还有一项未确认的要求"}
			}
			if err := worker.saveClaim(ctx, run); err != nil {
				t.Fatal(err)
			}
			if tc.wait {
				wait, err := json.Marshal(control.RunEvent{Kind: "waiting", WaitForTaskIDs: []string{"child-1"}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := worker.db.ExecContext(ctx, "INSERT INTO helper_waits(run_id,payload) VALUES (?,?)", run.ID, wait); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			service := &localHelper{}
			execute := func(_ context.Context, _, _, prompt, sessionID string, env []string, started func() error, session func(string) error) (string, error) {
				calls++
				if err := started(); err != nil {
					return "", err
				}
				if err := session("session-1"); err != nil {
					return "", err
				}
				if calls == 1 && sessionID == "" && service.active != nil {
					if strings.Contains(prompt, inputReceiptMarker) == tc.noInput || strings.Contains(prompt, "还有一项未确认的要求") == tc.noInput ||
						strings.Contains(prompt, "保留已完成的工作") == tc.noInput {
						t.Fatalf("原始提示词的回执指令或待核对输入不匹配 %q", prompt)
					}
					if tc.cancel {
						cancel()
					}
					return tc.initial, tc.failure
				}
				if calls == 2 && tc.repair != "" && sessionID == "session-1" && len(env) == 0 && service.active == nil {
					var raw string
					if err := worker.db.QueryRowContext(ctx, "SELECT body FROM input_receipt_original_replies WHERE run_id=?", run.ID).Scan(&raw); err != nil || raw != tc.initial {
						t.Fatalf("补写前应保存原回复 %q，错误 %v", raw, err)
					}
					if strings.Contains(prompt, "还有一项未确认的要求") || !strings.Contains(prompt, "禁止重做任务") {
						t.Fatalf("补写提示词重复了业务要求 %q", prompt)
					}
					return tc.repair, nil
				}
				t.Fatalf("调用 %d 使用了意外的会话或协作权限", calls)
				return "", nil
			}
			err = worker.execute(ctx, run, nil, service, execute)
			if tc.pending && !errors.Is(err, tc.failure) || !tc.pending && err != nil {
				t.Fatal(err)
			}
			// 执行环境可能已取消，通过独立上下文检查本机提交。
			ctx = context.Background()
			if tc.repair == "" && calls != 1 || tc.repair != "" && calls != 2 {
				t.Fatalf("意外的调用次数 %d", calls)
			}
			var payload []byte
			if err := worker.db.QueryRowContext(ctx, "SELECT payload FROM outbox WHERE run_id=? ORDER BY sequence DESC LIMIT 1", run.ID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var event control.RunEvent
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatal(err)
			}
			if tc.pending {
				if event.Kind != "session" || event.InputReceipt != nil {
					t.Fatalf("进程退出未确认时只应保留已宣布的会话 %+v", event)
				}
				return
			}
			var handled []string
			if event.InputReceipt != nil {
				handled = event.InputReceipt.HandledIDs
			}
			if event.Kind != tc.kind || event.SessionID != "session-1" || !reflect.DeepEqual(handled, tc.handled) {
				t.Fatalf("意外的终态 %+v，处理 ID %q", event, handled)
			}
			if event.Kind == "completed" && event.Text != original || strings.Contains(event.Text, inputReceiptMarker) {
				t.Fatalf("上报业务正文不匹配 %q", event.Text)
			}
			if event.Kind == "approval_requested" && (event.Approval == nil || event.Approval.Question != "请选择文件格式") {
				t.Fatalf("丢失请示 %+v", event.Approval)
			}
			var raw string
			err = worker.db.QueryRowContext(ctx, "SELECT body FROM input_receipt_original_replies WHERE run_id=?", run.ID).Scan(&raw)
			if tc.persist && (err != nil || raw != tc.initial) || !tc.persist && !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("终态提交后原回复 %q，错误 %v", raw, err)
			}
		})
	}
}

func TestInputReceiptRepairUnconfirmedExit(t *testing.T) {
	ctx := context.Background()
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	run := control.Run{ID: "run-1", TaskID: "task-1", InputIDs: []string{inputTestMessage}}
	if err := worker.saveClaim(ctx, run); err != nil {
		t.Fatal(err)
	}
	reply := "原始业务结果"
	event := control.RunEvent{Kind: "completed", SessionID: "session-1", Text: reply}
	err = worker.finishWithInputReceipt(ctx, run, "", func(context.Context, string, string, string, string, []string, func() error, func(string) error) (string, error) {
		return "", runner.ErrProcessExitUnconfirmed
	}, &event, false)
	if !errors.Is(err, runner.ErrProcessExitUnconfirmed) || event.Kind != "interrupted" {
		t.Fatalf("补写进程树退出尚未确认，事件 %+v，错误 %v", event, err)
	}
	var raw string
	if err := worker.db.QueryRowContext(ctx, "SELECT body FROM input_receipt_original_replies WHERE run_id=?", run.ID).Scan(&raw); err != nil || raw != reply {
		t.Fatalf("原回复 %q，错误 %v", raw, err)
	}
}
