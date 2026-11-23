package postgres

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/inputreceipt"
	"github.com/jackc/pgx/v5"
)

func TestInputReceiptAfterStopUsesCurrentRun(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	claim := func(want string, supported bool) workspace.RunClaim {
		t.Helper()
		run, err := store.ClaimRunWithInputReceipts(ctx, "home", token, true, false, false, supported)
		check(err)
		if run.ID != want {
			t.Fatalf("claim: got %q want %q", run.ID, want)
		}
		return run
	}
	record := func(run workspace.RunClaim, sequence int64, event workspace.RunEvent) error {
		return recordTaskControlEvent(ctx, store, "home", token, run, sequence, event)
	}
	requireStatus := func(id, wantStatus, wantRun string) {
		t.Helper()
		detail, err := store.TaskDetail(ctx, "root")
		check(err)
		for _, input := range detail.Inputs {
			if input.ID == id {
				if input.ReceiptStatus != wantStatus || input.ReceiptRunID != wantRun || (input.HandledAt != nil) != (wantStatus == "handled") {
					t.Fatalf("receipt expected %s in %s: %+v", wantStatus, wantRun, input)
				}
				return
			}
		}
		t.Fatalf("input %q missing from task details", id)
	}
	original := claim("root-run", true)
	check(record(original, 1, workspace.RunEvent{Kind: "started", SessionID: "native"}))
	_, err := store.SubmitMessage(ctx, workspace.Message{ID: "follow-up", TaskID: "root", Text: "Use CSV"})
	check(err)
	delivery, err := store.ClaimDelivery(ctx)
	check(err)
	const prompt = "Export data in CSV"
	_, err = store.Dispatch(ctx, delivery.ID, workspace.Dispatch{TaskID: "root", Lease: delivery.Lease, Prompt: prompt})
	check(err)
	check(record(original, 2, workspace.RunEvent{Kind: "completed", Text: "First result"}))
	requireStatus("follow-up", "awaiting_delivery", "")

	// 一个已关联合集的交接附件可先于原生进程得到设备收件确认。
	check(store.writeWorkspace(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO muster_transfers(id,run_id,task_id,source_device_id,target_device_id,name,kind,sha256,size)
			VALUES('transfer-sheet','root-run','root','home','home','sheet.csv','file',repeat('a',64),1);
			INSERT INTO muster_run_transfers(run_id,transfer_id,direction) VALUES('run-follow-up','transfer-sheet','input');
		`, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		return registerRunTransfers(ctx, tx, "run-follow-up")
	}))
	const attachmentID = "transfer:transfer-sheet"
	requireStatus(attachmentID, "awaiting_delivery", "")
	_, err = store.pool.Exec(ctx, `UPDATE muster_transfers SET received_at=clock_timestamp() WHERE id='transfer-sheet'`)
	check(err)
	requireStatus(attachmentID, "delivered", "")

	const inputID = "message:follow-up"
	first := claim("run-follow-up", true)
	if !slices.Equal(first.InputIDs, []string{inputID, attachmentID}) || len(first.InputMessages) != 0 ||
		len(first.Attachments) != 1 || first.Attachments[0].ID != "transfer-sheet" {
		t.Fatalf("follow-up ownership: %+v", first)
	}
	claim("", false)
	check(record(first, 1, workspace.RunEvent{Kind: "started"}))
	requireStatus("follow-up", "delivered", first.ID)
	controlTaskForTest(t, ctx, store, "root", "stop_current", firstControlRequest, "stopping")
	check(record(first, 2, workspace.RunEvent{Kind: "stopped"}))
	requireStatus("follow-up", "needs_review", first.ID)
	controlTaskForTest(t, ctx, store, "root", "resume", secondControlRequest, "queued")
	recovery := claim(manualRecoveryRunID("root", secondControlRequest), true)
	if recovery.SessionID != "native" || !slices.Equal(recovery.InputIDs, []string{inputID, attachmentID}) ||
		recovery.InputMessages[inputID] != prompt || strings.Contains(recovery.Prompt, prompt) ||
		len(recovery.Attachments) != 1 || recovery.Attachments[0].ID != "transfer-sheet" {
		t.Fatalf("restored follow-up: %+v", recovery)
	}

	result := workspace.RunEvent{Kind: "completed", Text: "Done", InputReceipt: &inputreceipt.Receipt{HandledIDs: []string{inputID, attachmentID}}}
	check(record(first, 3, result))
	requireStatus("follow-up", "needs_review", first.ID)
	requireStatus(attachmentID, "needs_review", first.ID)
	if err := record(recovery, 1, result); !errors.Is(err, workspace.ErrInvalidInputReceipt) {
		t.Fatalf("completion reported input handling before start: %v", err)
	}
	check(record(recovery, 1, workspace.RunEvent{Kind: "session", SessionID: "native"}))
	if err := record(recovery, 2, result); !errors.Is(err, workspace.ErrInvalidInputReceipt) {
		t.Fatalf("session event alone did not confirm input delivery: %v", err)
	}
	requireStatus("follow-up", "needs_review", first.ID)
	check(record(recovery, 2, workspace.RunEvent{Kind: "started"}))
	requireStatus("follow-up", "delivered", recovery.ID)
	if err := record(recovery, 3, workspace.RunEvent{Kind: "completed", Text: "No receipt"}); !errors.Is(err, workspace.ErrInvalidInputReceipt) {
		t.Fatalf("completion without receipt: %v", err)
	}
	check(record(recovery, 3, result))
	check(record(recovery, 3, result))
	requireStatus("follow-up", "handled", recovery.ID)
	requireStatus(attachmentID, "handled", recovery.ID)
}

func TestGitInputReceiptRequiresStartedEvent(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	_, err := store.pool.Exec(ctx, `UPDATE muster_runs SET status='completed' WHERE id='root-run'`)
	checkNotification(t, err)
	policy := gitdelivery.Policy{Revision: 1, BaseRef: "refs/heads/main", Remote: "origin"}
	_, err = store.pool.Exec(ctx, `UPDATE muster_tasks SET git_policy=$1,git_publish=true,status='completed',session_id='native'
		WHERE id='root'`, policy)
	checkNotification(t, err)
	_, err = store.SubmitMessage(ctx, workspace.Message{ID: "additional", TaskID: "root", Text: "追加要求"})
	checkNotification(t, err)
	delivery, err := store.ClaimDelivery(ctx)
	checkNotification(t, err)
	_, err = store.Dispatch(ctx, delivery.ID, workspace.Dispatch{TaskID: "root", Lease: delivery.Lease, Prompt: "处理追加要求"})
	checkNotification(t, err)
	run, err := store.ClaimRunWithInputReceipts(ctx, "home", token, true, true, true, true)
	checkNotification(t, err)
	if run.ID != "run-additional" || !slices.Equal(run.InputIDs, []string{"message:additional"}) {
		t.Fatalf("领取没有返回冻结的追加要求: %+v", run)
	}
	zero := uint32(0)
	request := gitdelivery.PushRequest{
		Generation: run.Generation, Lease: run.Lease, DestinationFingerprint: strings.Repeat("a", 64),
		Verification: gitdelivery.Verification{BaseOID: strings.Repeat("b", 40), CandidateOID: strings.Repeat("c", 40),
			TestExitCode: &zero, TestLogRef: "test-0/output.log", TestStatus: "passed"},
		InputReceipt: &inputreceipt.Receipt{HandledIDs: run.InputIDs},
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 1, workspace.RunEvent{Kind: "session", SessionID: "native"}))
	if _, err := store.AuthorizeGitPush(ctx, "home", token, run.ID, request); !errors.Is(err, workspace.ErrInvalidInputReceipt) {
		t.Fatalf("仅收到会话事件时不能确认输入或创建推送意图: %v", err)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 2, workspace.RunEvent{Kind: "started"}))
	intent, err := store.AuthorizeGitPush(ctx, "home", token, run.ID, request)
	checkNotification(t, err)
	replay, err := store.AuthorizeGitPush(ctx, "home", token, run.ID, request)
	checkNotification(t, err)
	if intent.ID == "" || !intent.PushAllowed || intent != replay {
		t.Fatalf("启动后创建和重放推送意图的结果不一致: %+v / %+v", intent, replay)
	}
}

func TestOversizedClaimReleasesDeviceWithoutDroppingInputs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		supported bool
	}{{"receipts", true}, {"legacy", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, store, token := openTaskControlFixture(t)
			_, err := store.pool.Exec(ctx, `
				INSERT INTO muster_messages(id,kind,body,task_id,status)
				SELECT 'prior-' || n,'user',repeat('<',6000),'root','handled' FROM generate_series(1,8) n;
				INSERT INTO muster_runs(id,task_id,input,device_id,status)
				SELECT 'run-prior-' || n,'root',repeat('<',6000),'home','interrupted' FROM generate_series(1,8) n;
				INSERT INTO muster_task_inputs(task_id,id,origin_run_id,message_id)
				SELECT 'root','message:prior-' || n,'run-prior-' || n,'prior-' || n FROM generate_series(1,8) n;
				INSERT INTO muster_tasks(id,project_id,project_name,device_id,directory_id,title,status,current_run_id,root_id,binding_id)
				VALUES ('other','project','Project','home','folder','Other','queued','other-run','other','project');
				INSERT INTO muster_runs(id,task_id,input,device_id) VALUES ('other-run','other','independent','home');
			`, pgx.QueryExecModeSimpleProtocol)
			checkNotification(t, err)
			blocked, err := store.ClaimRunWithInputReceipts(ctx, "home", token, true, false, false, tc.supported)
			checkNotification(t, err)
			if blocked.ID != "" {
				t.Fatalf("超限轮次不应返回给设备: %s", blocked.ID)
			}
			detail, err := store.TaskDetail(ctx, "root")
			checkNotification(t, err)
			if detail.Task.Status != "interrupted" || !strings.Contains(detail.Task.Result, "超过单轮传输上限") {
				t.Fatalf("任务未记录超限原因: %+v", detail.Task)
			}
			var pending int
			for _, input := range detail.Inputs {
				if input.ReceiptStatus == "awaiting_delivery" && input.HandledAt == nil && input.ReceiptRunID == "" {
					pending++
				}
			}
			if pending != 8 {
				t.Fatalf("应保留八条未处理输入，现有 %d 条", pending)
			}
			next, err := store.ClaimRunWithInputReceipts(ctx, "home", token, true, false, false, tc.supported)
			checkNotification(t, err)
			if next.ID != "other-run" || next.Prompt != "independent" {
				t.Fatalf("设备未能领取后续独立任务: %+v", next)
			}
		})
	}
}

func TestClaimReissueOmitsOversizedDelegationSuggestions(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	_, err := store.pool.Exec(ctx, `
		INSERT INTO muster_messages(id,kind,body,task_id,status)
		SELECT 'prior-' || n,'user',repeat('<',6000),'root','handled' FROM generate_series(1,7) n;
		INSERT INTO muster_runs(id,task_id,input,device_id,status)
		SELECT 'run-prior-' || n,'root',repeat('<',6000),'home','interrupted' FROM generate_series(1,7) n;
		INSERT INTO muster_task_inputs(task_id,id,origin_run_id,message_id)
		SELECT 'root','message:prior-' || n,'run-prior-' || n,'prior-' || n FROM generate_series(1,7) n;
	`, pgx.QueryExecModeSimpleProtocol)
	checkNotification(t, err)
	first, err := store.ClaimRunWithInputReceipts(ctx, "home", token, true, false, false, true)
	checkNotification(t, err)
	if first.ID != "root-run" || len(first.InputMessages) != 7 {
		t.Fatalf("未领取全部七条输入: id=%s, inputs=%d", first.ID, len(first.InputMessages))
	}
	_, err = store.pool.Exec(ctx, `
		INSERT INTO muster_devices(device_id,token_hash,name,hostname,os,arch)
		VALUES ('other',decode(repeat('02',32),'hex'),repeat('<',128),'office','windows','amd64');
		INSERT INTO muster_directories(device_id,directory_id,label)
		SELECT 'other','dir-' || n,repeat('<',120) FROM generate_series(1,10) n;
		UPDATE muster_projects SET name=repeat('<',120) WHERE id='project';
		INSERT INTO muster_project_bindings(id,project_id,device_id,directory_id,agent_kind,is_default)
		SELECT 'binding-' || n,'project','other','dir-' || n,'dsh',false FROM generate_series(1,10) n;
	`, pgx.QueryExecModeSimpleProtocol)
	checkNotification(t, err)
	reclaimed, err := store.ClaimRunWithInputReceipts(ctx, "home", token, true, false, false, true)
	checkNotification(t, err)
	if reclaimed.ID != first.ID || reclaimed.Lease != first.Lease || len(reclaimed.DelegationTargets) != 0 ||
		!slices.Equal(reclaimed.InputIDs, first.InputIDs) || len(reclaimed.InputMessages) != len(first.InputMessages) {
		t.Fatalf("重领应保留运行、租约和全部输入，并省略过大的设备建议: id=%s, inputs=%d, targets=%d",
			reclaimed.ID, len(reclaimed.InputMessages), len(reclaimed.DelegationTargets))
	}
}
