package postgres

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/inputreceipt"
	"github.com/jackc/pgx/v5"
)

func TestSessionBusyRetryPreservesFrozenClaim(t *testing.T) {
	ctx, address := newTestSchema(t)
	store, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	token := seedApprovalNotifications(t, ctx, store)
	approvalID := requestNotificationApproval(t, ctx, store, token, "CSV 或 JSON？")
	_, err = store.AnswerApproval(ctx, approvalID, "CSV")
	checkNotification(t, err)
	const promptTransferID = "transfer-prompt"
	const attachmentID = "transfer-file"
	resumeID := "run-" + stableUUID(approvalID)
	_, err = store.pool.Exec(ctx, `INSERT INTO muster_transfers
		(id,run_id,task_id,source_device_id,target_device_id,name,kind,sha256,size,received_at)
		VALUES ($1,'root-run','root','home','home','prompt.txt','prompt',repeat('a',64),1,clock_timestamp()),
		($2,'root-run','root','home','home','file.txt','file',repeat('b',64),1,clock_timestamp())`, promptTransferID, attachmentID)
	checkNotification(t, err)
	_, err = store.pool.Exec(ctx, `INSERT INTO muster_run_transfers(run_id,transfer_id,direction)
		VALUES ($1,$2,'input'),($1,$3,'input')`, resumeID, promptTransferID, attachmentID)
	checkNotification(t, err)
	_, err = store.pool.Exec(ctx, `UPDATE muster_runs SET prompt_transfer_id=$2,dependency_results=$3 WHERE id=$1`,
		resumeID, promptTransferID, []workspace.DependencyResult{{TaskID: "prerequisite", Status: "completed"}})
	checkNotification(t, err)
	initial, err := store.ClaimRunWithInputReceipts(ctx, "home", token, true, false, false, true)
	checkNotification(t, err)
	if initial.ID != resumeID || !initial.SessionFailureSupported || initial.SessionID != "native" || len(initial.Attachments) != 2 {
		t.Fatalf("续接轮领取错误 %+v", initial)
	}
	// 过期租约仍可用原轮凭据补报已退出的结果。不同的租约和代次不能结算原轮。
	_, err = store.pool.Exec(ctx, `UPDATE muster_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, initial.ID)
	checkNotification(t, err)
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, initial, 1, workspace.RunEvent{Kind: "started"}))
	failure := workspace.RunEvent{Kind: "failed", SessionFailure: "busy", Text: "原生会话正被占用"}
	invalid := initial
	invalid.Generation++
	if err := recordTaskControlEvent(ctx, store, "home", token, invalid, 2, failure); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("错误代次意外通过 %v", err)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, initial, 2, failure))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, initial, 2, failure))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, initial, 3, workspace.RunEvent{Kind: "session", SessionID: "native"}))
	limit, err := store.RunControl(ctx, "home", token, initial.ID, initial.Generation, initial.Lease)
	if limit || !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("旧轮仍可执行 %v %v", limit, err)
	}
	waiting, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if waiting.Task.Status != "queued" || waiting.Task.WaitingReason != "session_busy" || waiting.Task.ReportPending || len(waiting.Runs) != 3 {
		t.Fatalf("重复及迟到回执改变了重试状态 %+v", waiting)
	}
	claimTaskControlRun(t, ctx, store, "home", token, "")
	_, err = store.pool.Exec(ctx, `INSERT INTO muster_tasks(id,project_id,project_name,device_id,directory_id,title,status,current_run_id,root_id,binding_id)
		VALUES ('other','project','Project','home','folder','Independent','queued','other-run','other','project');
		INSERT INTO muster_runs(id,task_id,input,device_id) VALUES ('other-run','other','independent work','home')`, pgx.QueryExecModeSimpleProtocol)
	checkNotification(t, err)
	other := claimTaskControlRun(t, ctx, store, "home", token, "other-run")
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, other, 1, workspace.RunEvent{Kind: "completed", Text: "已完成"}))
	resumed, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(resumed.Close)
	claimTaskControlRun(t, ctx, resumed, "home", token, "")
	_, err = resumed.pool.Exec(ctx, `UPDATE muster_runs SET session_retry_after=clock_timestamp()-interval '1 second'
		WHERE task_id='root' AND status='queued'`)
	checkNotification(t, err)
	retry, err := resumed.ClaimRunWithInputReceipts(ctx, "home", token, true, false, false, true)
	checkNotification(t, err)
	if retry.ID == "" || retry.ID == initial.ID || retry.Generation <= initial.Generation || retry.Lease == initial.Lease ||
		retry.SessionID != initial.SessionID || retry.Prompt != initial.Prompt || retry.PromptTransferID != initial.PromptTransferID ||
		!reflect.DeepEqual(retry.Attachments, initial.Attachments) || !reflect.DeepEqual(retry.InputIDs, initial.InputIDs) ||
		!reflect.DeepEqual(retry.InputMessages, initial.InputMessages) || !reflect.DeepEqual(retry.DependencyResults, initial.DependencyResults) {
		t.Fatalf("重试丢失续接输入或复用了执行凭据\n初次 %+v\n重试 %+v", initial, retry)
	}
	checkNotification(t, recordTaskControlEvent(ctx, resumed, "home", token, retry, 1, workspace.RunEvent{Kind: "started"}))
	checkNotification(t, recordTaskControlEvent(ctx, resumed, "home", token, retry, 2, workspace.RunEvent{
		Kind: "completed", Text: "答复已处理", InputReceipt: &inputreceipt.Receipt{HandledIDs: retry.InputIDs},
	}))
	done, err := resumed.TaskDetail(ctx, "root")
	checkNotification(t, err)
	attachmentHandled := slices.ContainsFunc(done.Inputs, func(input workspace.TaskInput) bool {
		return input.ID == "transfer:"+attachmentID && input.ReceiptStatus == "handled" && input.ReceiptRunID == retry.ID
	})
	if done.Task.Status != "completed" || !done.Task.ReportPending || done.Approvals[0].Status != "handled" || !attachmentHandled {
		t.Fatalf("重试未结算答复或输入 %+v", done)
	}
}

func TestUnavailableSessionBlocksAdditionalWork(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	_, err := store.pool.Exec(ctx, `UPDATE muster_tasks SET session_id='native' WHERE id='root'`)
	checkNotification(t, err)
	first := claimTaskControlRun(t, ctx, store, "home", token, "root-run")
	_, err = store.SubmitMessage(ctx, workspace.Message{ID: "followup", TaskID: "root", Text: "追加工作"})
	checkNotification(t, err)
	delivery, err := store.ClaimDelivery(ctx)
	checkNotification(t, err)
	_, err = store.Dispatch(ctx, delivery.ID, workspace.Dispatch{TaskID: "root", Lease: delivery.Lease, Prompt: "追加工作"})
	checkNotification(t, err)
	_, err = store.SubmitMessage(ctx, workspace.Message{ID: "accepted", TaskID: "root", Text: "未派发的追加工作"})
	checkNotification(t, err)
	failure := workspace.RunEvent{Kind: "failed", SessionFailure: "unavailable", Text: "dsh 读到不兼容的会话"}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, first, 1, failure))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, first, 1, failure))
	claimTaskControlRun(t, ctx, store, "home", token, "")
	detail, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if detail.Task.Status != "interrupted" || detail.Task.WaitingReason != "session_unavailable" || len(detail.Runs) != 2 ||
		detail.Runs[0].SessionID != "native" || detail.Runs[0].Text != failure.Text || detail.Runs[1].Status != "interrupted" {
		t.Fatalf("会话损坏后未保留证据或冻结追加运行 %+v", detail)
	}
	_, err = store.SubmitMessage(ctx, workspace.Message{ID: "late", TaskID: "root", Text: "较晚的追加工作"})
	if !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("原任务仍接受追加 %v", err)
	}
	report, err := store.ClaimDelivery(ctx)
	checkNotification(t, err)
	if report.Kind != "report" || report.TaskID != "root" {
		t.Fatalf("未派发的追加要求阻塞不可用汇报 %+v", report)
	}
}

func TestSessionBusyRetryDoesNotConsumeLaterFollowUp(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	_, err := store.pool.Exec(ctx, `UPDATE muster_runs SET status='completed' WHERE id='root-run';
		UPDATE muster_tasks SET status='completed',session_id='native' WHERE id='root'`, pgx.QueryExecModeSimpleProtocol)
	checkNotification(t, err)
	queue := func(id, prompt string) {
		t.Helper()
		_, err := store.SubmitMessage(ctx, workspace.Message{ID: id, TaskID: "root", Text: prompt})
		checkNotification(t, err)
		delivery, err := store.ClaimDelivery(ctx)
		checkNotification(t, err)
		_, err = store.Dispatch(ctx, delivery.ID, workspace.Dispatch{TaskID: "root", Lease: delivery.Lease, Prompt: prompt})
		checkNotification(t, err)
		checkNotification(t, store.CompleteDelivery(ctx, delivery.ID, workspace.Completion{Lease: delivery.Lease, Status: "handled"}))
	}
	claim := func() workspace.RunClaim {
		t.Helper()
		run, err := store.ClaimRunWithInputReceipts(ctx, "home", token, true, false, false, true)
		checkNotification(t, err)
		return run
	}
	queue("first", "先处理第一个要求")
	busy := claim()
	queue("next", "再处理第二个要求")
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, busy, 1, workspace.RunEvent{Kind: "started"}))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, busy, 2, workspace.RunEvent{Kind: "failed", SessionFailure: "busy"}))
	_, err = store.pool.Exec(ctx, `UPDATE muster_runs SET session_retry_after=clock_timestamp()-interval '1 second'
		WHERE task_id='root' AND session_retry_origin=$1`, busy.ID)
	checkNotification(t, err)
	retry := claim()
	if retry.Prompt != busy.Prompt || len(retry.InputMessages) != 0 || !slices.Equal(retry.InputIDs, []string{"message:first"}) {
		t.Fatalf("重试提示重复或纳入后续要求 %+v", retry)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, retry, 1, workspace.RunEvent{Kind: "started"}))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, retry, 2, workspace.RunEvent{
		Kind: "completed", Text: "第一个要求已处理", InputReceipt: &inputreceipt.Receipt{HandledIDs: retry.InputIDs},
	}))
	later := claim()
	if later.ID != "run-next" || later.Generation <= retry.Generation || !slices.Equal(later.InputIDs, []string{"message:next"}) {
		t.Fatalf("重试结算后未独立派发后续要求 %+v", later)
	}
}

func TestSessionFailurePreservesPreviousInputDelivery(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	_, err := store.pool.Exec(ctx, `INSERT INTO muster_messages(id,kind,body,task_id,status)
		VALUES ('input-message','user','input','root','handled');
		INSERT INTO muster_task_inputs(task_id,id,origin_run_id,message_id)
		VALUES ('root','message:input-message','root-run','input-message')`, pgx.QueryExecModeSimpleProtocol)
	checkNotification(t, err)
	first, err := store.ClaimRunWithInputReceipts(ctx, "home", token, true, false, false, true)
	checkNotification(t, err)
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, first, 1,
		workspace.RunEvent{Kind: "started", SessionID: "native"}))
	controlTaskForTest(t, ctx, store, "root", "stop_current", firstControlRequest, "stopping")
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, first, 2, workspace.RunEvent{Kind: "stopped"}))
	controlTaskForTest(t, ctx, store, "root", "resume", secondControlRequest, "queued")
	resume, err := store.ClaimRunWithInputReceipts(ctx, "home", token, true, false, false, true)
	checkNotification(t, err)
	if resume.SessionID != "native" || len(resume.InputIDs) != 1 || !slices.Equal(resume.InputIDs, first.InputIDs) {
		t.Fatalf("续接轮未复用待核对输入 %+v", resume)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, resume, 1, workspace.RunEvent{Kind: "started"}))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, resume, 2, workspace.RunEvent{Kind: "started"}))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, resume, 3,
		workspace.RunEvent{Kind: "failed", SessionFailure: "busy"}))
	detail, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if !slices.ContainsFunc(detail.Inputs, func(input workspace.TaskInput) bool {
		return input.ID == "input-message" && input.ReceiptStatus == "needs_review" && input.ReceiptRunID == first.ID
	}) {
		t.Fatalf("占用重试覆盖了此前待核对的送达证据 %+v", detail.Inputs)
	}
}

func TestSessionBusyRetryHonorsTaskControl(t *testing.T) {
	for _, action := range []string{"stop_current", "cancel"} {
		t.Run(action, func(t *testing.T) {
			ctx, store, token := openTaskControlFixture(t)
			_, err := store.pool.Exec(ctx, `UPDATE muster_tasks SET session_id='native' WHERE id='root'`)
			checkNotification(t, err)
			first := claimTaskControlRun(t, ctx, store, "home", token, "root-run")
			checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, first, 1, workspace.RunEvent{Kind: "started"}))
			requested, settled := "stopping", "paused"
			if action == "cancel" {
				requested, settled = "cancelling", "cancelled"
			}
			controlTaskForTest(t, ctx, store, "root", action, firstControlRequest, requested)
			failure := workspace.RunEvent{Kind: "failed", SessionFailure: "busy"}
			checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, first, 2, failure))
			detail, err := store.TaskDetail(ctx, "root")
			checkNotification(t, err)
			if detail.Task.Status != settled {
				t.Fatalf("控制指令和会话故障交错后状态错误 %+v", detail.Task)
			}
			_, err = store.pool.Exec(ctx, `UPDATE muster_runs SET session_retry_after=clock_timestamp()-interval '1 second' WHERE status='queued'`)
			checkNotification(t, err)
			claimTaskControlRun(t, ctx, store, "home", token, "")
			if action == "stop_current" {
				controlTaskForTest(t, ctx, store, "root", "resume", secondControlRequest, "queued")
				claimTaskControlRun(t, ctx, store, "home", token, detail.Runs[1].ID)
			} else if len(detail.Runs) != 1 {
				t.Fatalf("取消任务仍创建了重试轮 %+v", detail.Runs)
			}
		})
	}
}

func TestSessionFailureCannotReplaceStartedOrUncertainRun(t *testing.T) {
	for _, scenario := range []string{"first_run", "adopted", "push_intent", "codex"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, store, token := openTaskControlFixture(t)
			if scenario != "first_run" {
				_, err := store.pool.Exec(ctx, `UPDATE muster_tasks SET session_id='native' WHERE id='root'`)
				checkNotification(t, err)
			}
			if scenario == "codex" {
				_, err := store.pool.Exec(ctx, `UPDATE muster_tasks SET agent_kind='codex' WHERE id='root'`)
				checkNotification(t, err)
			}
			run := claimTaskControlRun(t, ctx, store, "home", token, "root-run")
			if scenario == "adopted" {
				checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 1, workspace.RunEvent{Kind: "session", SessionID: "native"}))
			}
			if scenario == "push_intent" {
				_, err := store.pool.Exec(ctx, `INSERT INTO muster_git_push_intents
					(id,run_id,task_id,policy_revision,remote,target_ref,destination_fingerprint,verification)
					VALUES ('push','root-run','root',1,'origin','refs/heads/example',repeat('c',64),'{}')`)
				checkNotification(t, err)
			}
			if err := recordTaskControlEvent(ctx, store, "home", token, run, 2, workspace.RunEvent{Kind: "failed", SessionFailure: "busy"}); !errors.Is(err, workspace.ErrConflict) {
				t.Fatalf("不允许重试的轮次接受会话占用回执 %v", err)
			}
			claimed := claimTaskControlRun(t, ctx, store, "home", token, run.ID)
			if claimed.Lease != run.Lease {
				t.Fatalf("原轮租约改变 %+v", claimed)
			}
		})
	}
}
