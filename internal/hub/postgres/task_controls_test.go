package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

const (
	firstControlRequest  = "20000000-0000-4000-8000-000000000001"
	secondControlRequest = "20000000-0000-4000-8000-000000000002"
)

func TestTaskControlPauseQueuesApprovalAnswer(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	approvalID := requestNotificationApproval(t, ctx, store, token, "CSV 或 JSON？")
	controlTaskForTest(t, ctx, store, "root", "pause", firstControlRequest, "paused")
	answer, err := store.AnswerApproval(ctx, approvalID, "CSV")
	checkNotification(t, err)
	if answer.Status != "answered" || answer.Answer != "CSV" {
		t.Fatalf("暂停时没有保存答复: %+v", answer)
	}
	paused, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if paused.Task.Status != "paused" || paused.Task.ReportPending || len(paused.Runs) != 2 || paused.Runs[1].Status != "queued" {
		t.Fatalf("暂停时答复轮次未排队: %+v", paused)
	}
	claimTaskControlRun(t, ctx, store, "home", token, "")
	controlTaskForTest(t, ctx, store, "root", "resume", secondControlRequest, "queued")
	resumed := claimTaskControlRun(t, ctx, store, "home", token, paused.Runs[1].ID)
	if resumed.SessionID != "native" || !strings.Contains(resumed.Prompt, "CSV") {
		t.Fatalf("恢复时没有向原生会话提交答复: %+v", resumed)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, resumed, 1, workspace.RunEvent{Kind: "completed", Text: "已处理答复"}))
	completed, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if completed.Task.Status != "completed" || completed.Approvals[0].Status != "handled" {
		t.Fatalf("答复处理后任务状态错误: %+v", completed)
	}
}

func TestTaskControlCancelWaitsForClaimedDescendant(t *testing.T) {
	ctx, store, home := openTaskControlFixture(t)
	var office [32]byte
	office[0] = 2
	_, err := store.pool.Exec(ctx, `INSERT INTO muster_devices(device_id,token_hash,name,hostname,os,arch)
		VALUES ('office',$1,'Office','office','windows','amd64')`, office[:])
	checkNotification(t, err)
	_, err = store.pool.Exec(ctx, `
		INSERT INTO muster_tasks(id,project_id,project_name,device_id,directory_id,title,status,current_run_id,root_id,parent_id,binding_id)
		VALUES ('child','project','Project','office','folder','Claimed child','queued','child-run','root','root','project'),
		       ('queued-child','project','Project','office','folder','Queued child','queued','queued-child-run','root','root','project');
		INSERT INTO muster_runs(id,task_id,input,device_id)
		VALUES ('child-run','child','child input','office'),('queued-child-run','queued-child','other input','office');
		INSERT INTO muster_delegations(parent_run_id,request_key,child_task_id,request_digest)
		VALUES ('root-run','child-key','child','\x00'),('root-run','queued-child-key','queued-child','\x01');
	`, pgx.QueryExecModeSimpleProtocol)
	checkNotification(t, err)
	root := claimTaskControlRun(t, ctx, store, "home", home, "root-run")
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, root, 1, workspace.RunEvent{Kind: "started", SessionID: "native"}))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, root, 2, workspace.RunEvent{
		Kind: "waiting", WaitForTaskIDs: []string{"child", "queued-child"},
	}))
	child := claimTaskControlRun(t, ctx, store, "office", office, "child-run")
	checkNotification(t, recordTaskControlEvent(ctx, store, "office", office, child, 1, workspace.RunEvent{Kind: "started", SessionID: "child-native"}))
	controlTaskForTest(t, ctx, store, "root", "cancel", firstControlRequest, "cancelling")
	detail, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if detail.Task.ReportPending || len(detail.Children) != 2 || detail.Children[0].Status != "cancelling" || detail.Children[1].Status != "cancelled" {
		t.Fatalf("取消树时没有区分已领取和排队的子任务: %+v", detail)
	}
	queued, err := store.TaskDetail(ctx, "queued-child")
	checkNotification(t, err)
	if len(queued.Runs) != 1 || queued.Runs[0].Status != "cancelled" {
		t.Fatalf("排队轮次没有取消: %+v", queued.Runs)
	}
	stopping, err := store.RunControl(ctx, "office", office, child.ID, child.Generation, child.Lease)
	checkNotification(t, err)
	if !stopping {
		t.Fatal("已领取的后代没有收到停止指令")
	}
	claimTaskControlRun(t, ctx, store, "home", home, "")
	checkNotification(t, recordTaskControlEvent(ctx, store, "office", office, child, 2, workspace.RunEvent{Kind: "stopped"}))
	controlTaskForTest(t, ctx, store, "root", "cancel", firstControlRequest, "cancelled")
	claimTaskControlRun(t, ctx, store, "home", home, "")
	claimTaskControlRun(t, ctx, store, "office", office, "")
	detail, err = store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if len(detail.Runs) != 1 || detail.Children[0].Status != "cancelled" || detail.Children[1].Status != "cancelled" {
		t.Fatalf("取消已确认后子树仍包含未结束任务或多余的续接轮次: %+v", detail)
	}
}

func TestTaskControlStopWithoutNativeSession(t *testing.T) {
	t.Run("before launch", func(t *testing.T) {
		ctx, store, token := openTaskControlFixture(t)
		run := claimTaskControlRun(t, ctx, store, "home", token, "root-run")
		controlTaskForTest(t, ctx, store, "root", "stop_current", firstControlRequest, "stopping")
		checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 1, workspace.RunEvent{Kind: "stopped"}))
		controlTaskForTest(t, ctx, store, "root", "resume", secondControlRequest, "queued")
		detail, err := store.TaskDetail(ctx, "root")
		checkNotification(t, err)
		if len(detail.Runs) != 2 || detail.Runs[0].Status != "stopped" {
			t.Fatalf("停止前的输入未安排恢复: %+v", detail.Runs)
		}
		resumed := claimTaskControlRun(t, ctx, store, "home", token, detail.Runs[1].ID)
		if resumed.SessionID != "" || resumed.Prompt != run.Prompt || resumed.Generation <= run.Generation {
			t.Fatalf("恢复时没有完整保留未启动轮次的输入: %+v", resumed)
		}

		// 已确认退出的旧轮次可以补报审计记录，但不能推进新轮次或生成汇报。
		checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 2, workspace.RunEvent{Kind: "completed", Text: "迟到结果"}))
		_, err = store.RunControl(ctx, "home", token, run.ID, run.Generation, run.Lease)
		if !errors.Is(err, workspace.ErrConflict) {
			t.Fatalf("旧运行仍能查询当前运行指令: %v", err)
		}
		checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, resumed, 1, workspace.RunEvent{Kind: "started", SessionID: "new-native"}))
		detail, err = store.TaskDetail(ctx, "root")
		checkNotification(t, err)
		if detail.Task.Status != "running" || detail.Task.ReportPending || detail.Task.Result == "迟到结果" ||
			detail.Task.CurrentRunGeneration != resumed.Generation || detail.Runs[0].Status != "stopped" {
			t.Fatalf("旧运行的事件影响了当前执行: %+v", detail)
		}
	})

	t.Run("after launch", func(t *testing.T) {
		ctx, store, token := openTaskControlFixture(t)
		run := claimTaskControlRun(t, ctx, store, "home", token, "root-run")
		checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 1, workspace.RunEvent{Kind: "started"}))
		controlTaskForTest(t, ctx, store, "root", "stop_current", firstControlRequest, "stopping")
		checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 2, workspace.RunEvent{Kind: "stopped"}))
		detail, err := store.TaskDetail(ctx, "root")
		checkNotification(t, err)
		if detail.Task.Status != "interrupted" || slices.Contains(detail.Task.ControlActions, "resume") || len(detail.Runs) != 1 {
			t.Fatalf("已启动且缺失原生会话的停止轮次进入可重跑状态: %+v", detail)
		}
		_, err = store.ControlTask(ctx, "root", workspace.TaskControl{Action: "resume", RequestID: secondControlRequest})
		if !errors.Is(err, workspace.ErrConflict) {
			t.Fatalf("无法核对副作用的任务可自动重新执行: %v", err)
		}
		claimTaskControlRun(t, ctx, store, "home", token, "")
	})
}

func TestTaskControlInterruptedChildResumesParentOnce(t *testing.T) {
	ctx, store, home := openTaskControlFixture(t)
	var office [32]byte
	office[0] = 2
	_, err := store.pool.Exec(ctx, `INSERT INTO muster_devices(device_id,token_hash,name,hostname,os,arch)
		VALUES ('office',$1,'Office','office','windows','amd64')`, office[:])
	checkNotification(t, err)
	_, err = store.pool.Exec(ctx, `
		INSERT INTO muster_tasks(id,project_id,project_name,device_id,directory_id,title,status,current_run_id,root_id,parent_id,binding_id)
		VALUES ('child','project','Project','office','folder','Child','queued','child-run','root','root','project');
		INSERT INTO muster_runs(id,task_id,input,device_id) VALUES ('child-run','child','child input','office');
		INSERT INTO muster_delegations(parent_run_id,request_key,child_task_id,request_digest)
		VALUES ('root-run','child-key','child','\x00');
	`, pgx.QueryExecModeSimpleProtocol)
	checkNotification(t, err)
	root := claimTaskControlRun(t, ctx, store, "home", home, "root-run")
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, root, 1, workspace.RunEvent{Kind: "started", SessionID: "native"}))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, root, 2, workspace.RunEvent{
		Kind: "waiting", WaitForTaskIDs: []string{"child"},
	}))
	claimTaskControlRun(t, ctx, store, "home", home, "")
	child := claimTaskControlRun(t, ctx, store, "office", office, "child-run")
	checkNotification(t, recordTaskControlEvent(ctx, store, "office", office, child, 1, workspace.RunEvent{Kind: "started"}))
	controlTaskForTest(t, ctx, store, "child", "stop_current", firstControlRequest, "stopping")
	stopped := workspace.RunEvent{Kind: "stopped"}
	checkNotification(t, recordTaskControlEvent(ctx, store, "office", office, child, 2, stopped))
	beforeReplay, err := store.Workspace(ctx)
	checkNotification(t, err)
	checkNotification(t, recordTaskControlEvent(ctx, store, "office", office, child, 2, stopped))
	afterReplay, err := store.Workspace(ctx)
	checkNotification(t, err)
	if beforeReplay.Cursor != afterReplay.Cursor {
		t.Fatalf("重放停止回执推进了游标: %d -> %d", beforeReplay.Cursor, afterReplay.Cursor)
	}
	detail, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if detail.Task.Status != "queued" || detail.Task.ReportPending || len(detail.Runs) != 2 ||
		len(detail.Children) != 1 || detail.Children[0].Status != "interrupted" {
		t.Fatalf("子任务停止后父任务未登记一次续接: %+v", detail)
	}
	resumed := claimTaskControlRun(t, ctx, store, "home", home, detail.Runs[1].ID)
	if resumed.SessionID != "native" || !slices.Equal(resumed.DependencyResults, []workspace.DependencyResult{
		{TaskID: "child", Status: "interrupted"},
	}) {
		t.Fatalf("父任务续接缺少原生会话或子任务终态: %+v", resumed)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, resumed, 1, workspace.RunEvent{Kind: "completed", Text: "已处理"}))
	claimTaskControlRun(t, ctx, store, "home", home, "")
}

func TestTaskControlCancelWaitsForLegacyDeviceExit(t *testing.T) {
	ctx, store, token := openTaskControlFixture(t)
	run, err := store.ClaimRun(ctx, "home", token)
	checkNotification(t, err)
	if run.ID != "root-run" || run.ControlSupported {
		t.Fatalf("旧设备的领取结果不正确: %+v", run)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 1, workspace.RunEvent{Kind: "started", SessionID: "native"}))
	_, err = store.ControlTask(ctx, "root", workspace.TaskControl{Action: "stop_current", RequestID: firstControlRequest})
	if !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("缺少控制能力的设备收到了停止指令: %v", err)
	}
	controlTaskForTest(t, ctx, store, "root", "cancel", secondControlRequest, "cancelling")
	_, err = store.RenewRun(ctx, "home", token, run.ID, run.Generation, run.Lease)
	checkNotification(t, err)
	reclaimed, err := store.ClaimRun(ctx, "home", token)
	checkNotification(t, err)
	if reclaimed.ID != run.ID || reclaimed.Lease != run.Lease {
		t.Fatalf("取消时设备无法重领当前轮次: %+v", reclaimed)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", token, run, 2, workspace.RunEvent{Kind: "completed", Text: "旧设备自然退出"}))
	controlTaskForTest(t, ctx, store, "root", "cancel", secondControlRequest, "cancelled")
	detail, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if detail.Task.ReportPending || len(detail.Runs) != 1 {
		t.Fatalf("旧设备退出后生成了多余轮次或汇报: %+v", detail)
	}
	claimTaskControlRun(t, ctx, store, "home", token, "")
}

func openTaskControlFixture(t *testing.T) (context.Context, *InventoryStore, [32]byte) {
	t.Helper()
	ctx, address := newTestSchema(t)
	store, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	return ctx, store, seedApprovalNotifications(t, ctx, store)
}

func controlTaskForTest(t *testing.T, ctx context.Context, store *InventoryStore, taskID, action, requestID, wantStatus string) {
	t.Helper()
	task, err := store.ControlTask(ctx, taskID, workspace.TaskControl{Action: action, RequestID: requestID})
	checkNotification(t, err)
	if task.Status != wantStatus {
		t.Fatalf("%s %s 后状态为 %q，预期 %q", action, taskID, task.Status, wantStatus)
	}
}

func claimTaskControlRun(t *testing.T, ctx context.Context, store *InventoryStore, device string, token [32]byte, wantID string) workspace.RunClaim {
	t.Helper()
	run, err := store.ClaimRunWithControl(ctx, device, token, true)
	checkNotification(t, err)
	if run.ID != wantID {
		t.Fatalf("设备 %s 领取了 %q，预期 %q", device, run.ID, wantID)
	}
	return run
}

func recordTaskControlEvent(ctx context.Context, store *InventoryStore, device string, token [32]byte, run workspace.RunClaim, seq int64, event workspace.RunEvent) error {
	event.Generation, event.Lease = run.Generation, run.Lease
	return store.RecordRunEvent(ctx, device, token, run.ID, seq, event)
}
