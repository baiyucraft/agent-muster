package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/artifact"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestCenterTaskGroupPersistsPlanAndReportsOnce(t *testing.T) {
	ctx, address, store, home, office := openCenterGroupFixture(t)
	delivery := claimCenterGroupMessage(t, ctx, store)
	plan := workspace.Dispatch{Lease: delivery.Lease, Title: "整理项目资料", Steps: []workspace.DispatchStep{
		{ProjectID: "project", Title: "生成索引", Prompt: "列出资料"},
		{ProjectID: "project", BindingID: "office-binding", Title: "核对版本", Prompt: "找最新版本"},
		{ProjectID: "project", BindingID: "office-binding", Title: "汇总结论", Prompt: "汇总前两项", DependsOn: []int{2, 1}},
	}}
	invalid := plan
	invalid.Steps = slices.Clone(plan.Steps)
	invalid.Steps[2].ProjectID = "missing-project"
	if _, err := store.Dispatch(ctx, delivery.ID, invalid); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("无效项目的派发返回 %v，预期冲突", err)
	}
	before, err := store.Workspace(ctx)
	checkNotification(t, err)
	if len(before.Tasks) != 0 || before.Messages[0].Status != "accepted" {
		t.Fatalf("无效步骤导致任务组部分写入: %+v", before)
	}

	rootID, err := store.Dispatch(ctx, delivery.ID, plan)
	checkNotification(t, err)
	if _, err := store.SubmitMessage(ctx, workspace.Message{
		ID: "group-follow-up", TaskID: rootID, Text: "修改原生会话中的要求",
	}); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("组合根任务受理了无法续接的定向消息: %v", err)
	}
	group := readCenterGroup(t, ctx, store, rootID, "queued")
	first := centerGroupStep(t, group, "生成索引", "queued")
	second := centerGroupStep(t, group, "核对版本", "queued")
	third := centerGroupStep(t, group, "汇总结论", "waiting_dependencies")
	if len(group.Children) != 3 || !slices.Contains(third.DependsOnTaskIDs, first.ID) ||
		!slices.Contains(third.DependsOnTaskIDs, second.ID) || len(third.DependsOnTaskIDs) != 2 {
		t.Fatalf("任务组步骤或依赖关系错误: %+v", group.Children)
	}
	afterDispatch, err := store.Workspace(ctx)
	checkNotification(t, err)
	plan.Steps[2].DependsOn = []int{1, 2}
	repeatedID, err := store.Dispatch(ctx, delivery.ID, plan)
	checkNotification(t, err)
	if repeatedID != rootID {
		t.Fatalf("重试时根任务 ID 由 %q 变成 %q", rootID, repeatedID)
	}
	plan.Steps[2].Prompt = "另一份计划"
	if _, err := store.Dispatch(ctx, delivery.ID, plan); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("已派发的来源消息接受了不同计划: %v", err)
	}
	afterReplay, err := store.Workspace(ctx)
	checkNotification(t, err)
	if afterReplay.Cursor != afterDispatch.Cursor || len(afterReplay.Tasks) != 4 {
		t.Fatalf("派发重试重复写入任务或游标: %+v", afterReplay)
	}
	checkNotification(t, store.CompleteDelivery(ctx, delivery.ID, workspace.Completion{Lease: delivery.Lease, Status: "handled"}))

	homeRun := claimCenterGroupStep(t, ctx, store, "home", home, first)
	officeRun := claimCenterGroupStep(t, ctx, store, "office", office, second)
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, homeRun, 1,
		workspace.RunEvent{Kind: "started", SessionID: "home-native"}))
	checkNotification(t, recordTaskControlEvent(ctx, store, "office", office, officeRun, 1,
		workspace.RunEvent{Kind: "started", SessionID: "office-native"}))
	readCenterGroup(t, ctx, store, rootID, "running")
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, homeRun, 2, workspace.RunEvent{
		Kind: "approval_requested", Approval: &approval.Question{Question: "使用 CSV 还是 JSON？", Options: []string{"CSV", "JSON"}},
	}))
	checkNotification(t, recordTaskControlEvent(ctx, store, "office", office, officeRun, 2,
		workspace.RunEvent{Kind: "completed", Text: "已核对版本"}))
	waiting := readCenterGroup(t, ctx, store, rootID, "waiting_reply")
	if len(waiting.Approvals) != 1 || waiting.Approvals[0].TaskID != first.ID || waiting.Task.ReportPending {
		t.Fatalf("组内请示或汇报状态错误: %+v", waiting)
	}
	claimTaskControlRun(t, ctx, store, "office", office, "")
	_, err = store.AnswerApproval(ctx, waiting.Approvals[0].ID, "CSV")
	checkNotification(t, err)
	resumed := claimCenterGroupStep(t, ctx, store, "home", home, first)
	if resumed.ID == homeRun.ID || resumed.SessionID != "home-native" {
		t.Fatalf("前置步骤未在原生会话续接: %+v", resumed)
	}
	index := makeTestArtifact(resumed.ID, "文档/索引.csv")
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, resumed, 1,
		workspace.RunEvent{Kind: "started"}))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, resumed, 2,
		workspace.RunEvent{Kind: "completed", Text: "已生成 CSV 索引", Artifacts: []artifact.Reference{index}}))
	centerGroupStep(t, readCenterGroup(t, ctx, store, rootID, "queued"), third.Title, "queued")

	store.Close()
	reopened, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(reopened.Close)
	finalRun := claimCenterGroupStep(t, ctx, reopened, "office", office, third)
	if !strings.Contains(finalRun.Prompt, "汇总前两项") ||
		strings.Count(finalRun.Prompt, "已生成 CSV 索引") != 1 || strings.Count(finalRun.Prompt, "已核对版本") != 1 {
		t.Fatalf("重启后步骤的原始输入或前置结论缺失或重复: %+v", finalRun)
	}
	summary := makeTestArtifact(finalRun.ID, "说明.md")
	result := workspace.RunEvent{Kind: "completed", Text: "已汇总结论", Artifacts: []artifact.Reference{summary}}
	checkNotification(t, recordTaskControlEvent(ctx, reopened, "office", office, finalRun, 1,
		workspace.RunEvent{Kind: "started"}))
	checkNotification(t, recordTaskControlEvent(ctx, reopened, "office", office, finalRun, 2, result))
	checkNotification(t, recordTaskControlEvent(ctx, reopened, "office", office, finalRun, 2, result))
	finished := readCenterGroup(t, ctx, reopened, rootID, "completed")
	if !finished.Task.ReportPending || !strings.Contains(finished.Task.Result, "已生成 CSV 索引") ||
		!strings.Contains(finished.Task.Result, "已汇总结论") {
		t.Fatalf("任务组汇报缺少已完成步骤的结果: %+v", finished.Task)
	}
	firstDetail, err := reopened.TaskDetail(ctx, first.ID)
	checkNotification(t, err)
	if len(finished.Artifacts) != 2 || len(firstDetail.Artifacts) != 1 || firstDetail.Artifacts[0].Reference != index {
		t.Fatalf("汇总产物未完整登记或错归步骤: %+v / %+v", finished.Artifacts, firstDetail.Artifacts)
	}
	report, err := reopened.ClaimDelivery(ctx)
	checkNotification(t, err)
	if report.Kind != "report" || report.TaskID != rootID || !strings.Contains(report.Text, "已核对版本") ||
		!strings.Contains(report.Text, "home · "+index.Path) || !strings.Contains(report.Text, "office · "+summary.Path) {
		t.Fatalf("领取到错误的中心汇报: %+v", report)
	}
	completion := workspace.Completion{Lease: report.Lease, Status: "handled", Text: "资料处理完毕。"}
	checkNotification(t, reopened.CompleteDelivery(ctx, report.ID, completion))
	checkNotification(t, reopened.CompleteDelivery(ctx, report.ID, completion))
	remaining, err := reopened.ClaimDelivery(ctx)
	checkNotification(t, err)
	if remaining.ID != "" {
		t.Fatalf("中心任务组留下了额外的投递: %+v", remaining)
	}
	snapshot, err := reopened.Workspace(ctx)
	checkNotification(t, err)
	var reports []workspace.Message
	for _, message := range snapshot.Messages {
		if message.Kind == "report" {
			reports = append(reports, message)
		}
	}
	if len(reports) != 1 || reports[0].TaskID != rootID || reports[0].Text != completion.Text {
		t.Fatalf("主对话应只收到一条任务组汇报: %+v", reports)
	}
	if events, err := reopened.WorkspaceEvents(ctx, afterReplay.Cursor); err != nil || len(events) == 0 || events[len(events)-1] != snapshot.Cursor {
		t.Fatalf("从派发游标补读不到中心任务组的终态: %v, %v", events, err)
	}
}

func TestCenterTaskGroupSkipsTransitiveDependentsAfterFailure(t *testing.T) {
	ctx, _, store, home, office := openCenterGroupFixture(t)
	delivery := claimCenterGroupMessage(t, ctx, store)
	rootID, err := store.Dispatch(ctx, delivery.ID, workspace.Dispatch{Lease: delivery.Lease, Title: "部分失败的任务组", Steps: []workspace.DispatchStep{
		{ProjectID: "project", Title: "查找素材", Prompt: "查需要的素材"},
		{ProjectID: "project", BindingID: "office-binding", Title: "检查环境", Prompt: "检查环境"},
		{ProjectID: "project", Title: "整理素材", Prompt: "整理找到的素材", DependsOn: []int{1}},
		{ProjectID: "project", Title: "生成目录", Prompt: "列出整理后的素材", DependsOn: []int{3}},
	}})
	checkNotification(t, err)
	checkNotification(t, store.CompleteDelivery(ctx, delivery.ID, workspace.Completion{Lease: delivery.Lease, Status: "handled"}))
	group := readCenterGroup(t, ctx, store, rootID, "queued")
	pausedStep := centerGroupStep(t, group, "整理素材", "waiting_dependencies")
	controlTaskForTest(t, ctx, store, pausedStep.ID, "pause", firstControlRequest, "paused")
	homeRun := claimCenterGroupStep(t, ctx, store, "home", home, centerGroupStep(t, group, "查找素材", "queued"))
	officeRun := claimCenterGroupStep(t, ctx, store, "office", office, centerGroupStep(t, group, "检查环境", "queued"))
	checkNotification(t, recordTaskControlEvent(ctx, store, "office", office, officeRun, 1, workspace.RunEvent{Kind: "started"}))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, homeRun, 1,
		workspace.RunEvent{Kind: "failed", Text: "缺少源文件"}))
	current := readCenterGroup(t, ctx, store, rootID, "running")
	for _, title := range []string{"整理素材", "生成目录"} {
		step := centerGroupStep(t, current, title, "failed")
		if step.Result == "" || step.ReportPending {
			t.Fatalf("未执行的依赖步骤缺少失败原因或产生了独立汇报: %+v", step)
		}
	}
	claimTaskControlRun(t, ctx, store, "home", home, "")
	if current.Task.ReportPending {
		t.Fatalf("独立步骤仍在运行时产生了根任务汇报: %+v", current.Task)
	}
	checkNotification(t, recordTaskControlEvent(ctx, store, "office", office, officeRun, 2,
		workspace.RunEvent{Kind: "completed", Text: "环境通过"}))
	failed := readCenterGroup(t, ctx, store, rootID, "failed")
	if !failed.Task.ReportPending || !strings.Contains(failed.Task.Result, "缺少源文件") || !strings.Contains(failed.Task.Result, "环境通过") {
		t.Fatalf("根任务丢失了部分成功或失败的结果: %+v", failed.Task)
	}
}

func TestCenterTaskGroupCancellationWaitsForActiveStep(t *testing.T) {
	ctx, _, store, home, office := openCenterGroupFixture(t)
	delivery := claimCenterGroupMessage(t, ctx, store)
	rootID, err := store.Dispatch(ctx, delivery.ID, workspace.Dispatch{Lease: delivery.Lease, Title: "可取消的任务组", Steps: []workspace.DispatchStep{
		{ProjectID: "project", Title: "处理素材", Prompt: "处理素材"},
		{ProjectID: "project", BindingID: "office-binding", Title: "归档", Prompt: "归档", DependsOn: []int{1}},
	}})
	checkNotification(t, err)
	checkNotification(t, store.CompleteDelivery(ctx, delivery.ID, workspace.Completion{Lease: delivery.Lease, Status: "handled"}))
	group := readCenterGroup(t, ctx, store, rootID, "queued")
	step := centerGroupStep(t, group, "处理素材", "queued")
	run := claimCenterGroupStep(t, ctx, store, "home", home, step)
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, run, 1, workspace.RunEvent{Kind: "started", SessionID: "native"}))
	controlTaskForTest(t, ctx, store, rootID, "cancel", firstControlRequest, "cancelling")
	current := readCenterGroup(t, ctx, store, rootID, "cancelling")
	if current.Task.ReportPending {
		t.Fatal("设备仍在执行时提前创建了取消汇报")
	}
	centerGroupStep(t, current, "处理素材", "cancelling")
	centerGroupStep(t, current, "归档", "cancelled")
	claimTaskControlRun(t, ctx, store, "office", office, "")
	stop, err := store.RunControl(ctx, "home", home, run.ID, run.Generation, run.Lease)
	checkNotification(t, err)
	if !stop {
		t.Fatal("取消任务组未通知执行设备停止运行")
	}
	stopped := workspace.RunEvent{Kind: "stopped"}
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, run, 2, stopped))
	checkNotification(t, recordTaskControlEvent(ctx, store, "home", home, run, 2, stopped))
	controlTaskForTest(t, ctx, store, rootID, "cancel", firstControlRequest, "cancelled")
	settled := readCenterGroup(t, ctx, store, rootID, "cancelled")
	centerGroupStep(t, settled, "处理素材", "cancelled")
	if !settled.Task.ReportPending {
		t.Fatal("最后一个运行确认退出后未创建取消汇报")
	}
	report, err := store.ClaimDelivery(ctx)
	checkNotification(t, err)
	if report.Kind != "report" || report.TaskID != rootID || !strings.Contains(report.Text, "cancelled") {
		t.Fatalf("任务组取消收到错误汇报: %+v", report)
	}
	checkNotification(t, store.CompleteDelivery(ctx, report.ID, workspace.Completion{Lease: report.Lease, Status: "handled", Text: "已取消。"}))
	remaining, err := store.ClaimDelivery(ctx)
	checkNotification(t, err)
	if remaining.ID != "" {
		t.Fatalf("重复处理取消回执后出现额外投递: %+v", remaining)
	}
	claimTaskControlRun(t, ctx, store, "home", home, "")
}

// openCenterGroupFixture 使用独立 schema 注册两个设备及一个具备不同 Agent 绑定的项目。
func openCenterGroupFixture(t *testing.T) (context.Context, string, *InventoryStore, [32]byte, [32]byte) {
	t.Helper()
	ctx, address := newTestSchema(t)
	store, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	home, office := [32]byte{1}, [32]byte{2}
	_, err = store.pool.Exec(ctx, `INSERT INTO muster_devices(device_id,token_hash,name,hostname,os,arch)
		VALUES ('home',$1,'Home','home','windows','amd64'),('office',$2,'Office','office','windows','amd64')`, home[:], office[:])
	checkNotification(t, err)
	checkNotification(t, store.ReplaceDirectories(ctx, "home", home, []workspace.Directory{{ID: "home-dir", Label: "Home"}}))
	checkNotification(t, store.ReplaceDirectories(ctx, "office", office, []workspace.Directory{{ID: "office-dir", Label: "Office"}}))
	_, err = store.SaveProject(ctx, workspace.Project{ID: "project", Name: "Project", Bindings: []workspace.Binding{
		{ID: "home-binding", DeviceID: "home", DirectoryID: "home-dir", AgentKind: "dsh", IsDefault: true},
		{ID: "office-binding", DeviceID: "office", DirectoryID: "office-dir", AgentKind: "codex"},
	}})
	checkNotification(t, err)
	return ctx, address, store, home, office
}

func claimCenterGroupMessage(t *testing.T, ctx context.Context, store *InventoryStore) workspace.Delivery {
	t.Helper()
	message, err := store.SubmitMessage(ctx, workspace.Message{ID: "center-request", Text: "拆成多项设备任务"})
	checkNotification(t, err)
	delivery, err := store.ClaimDelivery(ctx)
	checkNotification(t, err)
	if delivery.MessageID != message.ID || delivery.Lease == "" {
		t.Fatalf("未领取到用于派发的用户消息: %+v", delivery)
	}
	return delivery
}

// readCenterGroup 同时检查聚合状态和根任务的无设备运行约束。
func readCenterGroup(t *testing.T, ctx context.Context, store *InventoryStore, id, status string) workspace.Detail {
	t.Helper()
	detail, err := store.TaskDetail(ctx, id)
	checkNotification(t, err)
	root := detail.Task
	if !root.CenterGroup || root.Status != status || root.RootTaskID != id || root.ParentTaskID != "" ||
		root.DeviceID != "" || root.BindingID != "" || root.ProjectID != "" || root.DirectoryID != "" ||
		root.CurrentRunGeneration != 0 || len(detail.Runs) != 0 {
		t.Fatalf("根任务组投影不符合预期状态 %q: %+v", status, detail)
	}
	return detail
}

func centerGroupStep(t *testing.T, group workspace.Detail, title, status string) workspace.Task {
	t.Helper()
	for _, step := range group.Children {
		if step.Title == title {
			if step.CenterGroup || step.Status != status || step.RootTaskID != group.Task.ID || step.ParentTaskID != group.Task.ID {
				t.Fatalf("中心步骤应属于根任务组并处于 %q: %+v", status, step)
			}
			return step
		}
	}
	t.Fatalf("缺少中心步骤 %q: %+v", title, group.Children)
	return workspace.Task{}
}

// claimCenterGroupStep 检查设备领取的绑定快照，以及中心根任务没有上游设备。
func claimCenterGroupStep(t *testing.T, ctx context.Context, store *InventoryStore, device string, token [32]byte, step workspace.Task) workspace.RunClaim {
	t.Helper()
	run, err := store.ClaimRunWithControl(ctx, device, token, true)
	checkNotification(t, err)
	if run.ID == "" || run.TaskID != step.ID || run.ParentDeviceID != "" || run.RootTaskID != step.RootTaskID ||
		run.BindingID != device+"-binding" || run.DirectoryID != device+"-dir" || run.AgentKind != step.AgentKind {
		t.Fatalf("设备 %s 领取了错误的中心步骤: %+v", device, run)
	}
	return run
}
