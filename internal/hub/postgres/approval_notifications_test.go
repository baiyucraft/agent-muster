package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/hub/im"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

func TestApprovalNotificationSurvivesRestart(t *testing.T) {
	ctx, address := newTestSchema(t)
	store, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	recipient := im.Recipient{AppID: "app-1", OpenID: "user-1"}
	token := seedApprovalNotifications(t, ctx, store)
	approvalID := requestNotificationApproval(t, ctx, store, token, "待答复的问题")
	if empty, err := store.ClaimApprovalNotification(ctx, recipient); err != nil || empty.Lease != "" {
		t.Fatalf("未配置时领取到通知: %+v, %v", empty, err)
	}
	checkNotification(t, store.EnableApprovalNotifications(ctx, recipient))
	first := claimNotification(t, ctx, store, recipient, approvalID, "pending", "")
	if empty, err := store.ClaimApprovalNotification(ctx, recipient); err != nil || empty.Lease != "" {
		t.Fatalf("同一租约下重复领取通知: %+v, %v", empty, err)
	}
	checkNotification(t, store.RetryApprovalNotification(ctx, recipient, first))
	if err := store.CompleteApprovalNotification(ctx, recipient, first, "discarded"); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("已释放的租约可以写入回执: %v", err)
	}
	_, err = store.pool.Exec(ctx, `UPDATE muster_approval_notifications SET next_attempt_at=clock_timestamp() WHERE approval_id=$1`, approvalID)
	checkNotification(t, err)
	second := claimNotification(t, ctx, store, recipient, approvalID, "pending", "")
	if second.Lease == first.Lease {
		t.Fatal("重试未签发新租约")
	}
	const providerID = "message-1"
	option := 1
	reply := im.Reply{ApprovalID: approvalID, OpenID: recipient.OpenID, MessageID: providerID, Option: &option}
	if _, err := store.AnswerApprovalFromIM(ctx, recipient, reply); !errors.Is(err, im.ErrReplyUnavailable) {
		t.Fatalf("尚未保存原消息 ID 时接受了答复: %v", err)
	}

	// 发送响应丢失后，即使 Web 先保存答复，仍重试原始待答复卡片。
	answer, err := store.AnswerApproval(ctx, approvalID, "实际答复")
	checkNotification(t, err)
	if answer.Status != "answered" || !answer.WaitingDevice {
		t.Fatalf("Web 答复状态不正确: %+v", answer)
	}
	_, err = store.pool.Exec(ctx, `UPDATE muster_approval_notifications
		SET lease_until=clock_timestamp()-interval '1 second' WHERE approval_id=$1`, approvalID)
	checkNotification(t, err)
	store.Close()
	store, err = OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	checkNotification(t, store.EnableApprovalNotifications(ctx, recipient))
	replayed := claimNotification(t, ctx, store, recipient, approvalID, "pending", "")
	if replayed.Approval.Answer != "" || replayed.Approval.AnsweredAt != nil || replayed.Lease == second.Lease || replayed.Approval.WaitingDevice {
		t.Fatalf("重试初始通知时没有保留原始卡片快照: %+v", replayed)
	}
	if err := store.CompleteApprovalNotification(ctx, recipient, second, providerID); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("过期租约可以写入回执: %v", err)
	}
	checkNotification(t, store.CompleteApprovalNotification(ctx, recipient, replayed, providerID))
	answered := claimNotification(t, ctx, store, recipient, approvalID, "answered", providerID)
	if answered.Approval.Answer != "实际答复" || !answered.Approval.WaitingDevice {
		t.Fatalf("已答复卡片快照不正确: %+v", answered)
	}
	resumed, err := store.ClaimRun(ctx, "home", token)
	checkNotification(t, err)
	if resumed.SessionID != "native" {
		t.Fatalf("答复未续接原生会话: %+v", resumed)
	}
	checkNotification(t, store.RecordRunEvent(ctx, "home", token, resumed.ID, 1, workspace.RunEvent{
		Kind: "completed", Text: "已处理", Generation: resumed.Generation, Lease: resumed.Lease,
	}))
	checkNotification(t, store.CompleteApprovalNotification(ctx, recipient, answered, providerID))
	for _, age := range []string{"337 hours", "335 hours 59 minutes 50 seconds"} {
		_, err = store.pool.Exec(ctx, `UPDATE muster_approval_notifications
			SET first_send_at=clock_timestamp()-$2::interval WHERE approval_id=$1`, approvalID, age)
		checkNotification(t, err)
		if empty, err := store.ClaimApprovalNotification(ctx, recipient); err != nil || empty.Lease != "" {
			t.Fatalf("超出安全更新时限仍领取卡片 (已发送 %s): %+v, %v", age, empty, err)
		}
	}
	_, err = store.pool.Exec(ctx, `UPDATE muster_approval_notifications
		SET first_send_at=clock_timestamp()-interval '335 hours' WHERE approval_id=$1`, approvalID)
	checkNotification(t, err)
	handled := claimNotification(t, ctx, store, recipient, approvalID, "handled", providerID)
	if handled.Approval.HandledAt == nil {
		t.Fatal("已处理卡片缺少处理时间")
	}
	checkNotification(t, store.CompleteApprovalNotification(ctx, recipient, handled, providerID))
	if empty, err := store.ClaimApprovalNotification(ctx, recipient); err != nil || empty.Lease != "" {
		t.Fatalf("已确认的最终状态重复投递: %+v, %v", empty, err)
	}
	duplicate, err := store.AnswerApprovalFromIM(ctx, recipient, reply)
	checkNotification(t, err)
	if duplicate.Status != "handled" || duplicate.Answer != "实际答复" {
		t.Fatalf("迟到答复覆盖已保存的结果: %+v", duplicate)
	}
}

func TestApprovalNotificationRecipientAndConcurrentAnswer(t *testing.T) {
	ctx, address := newTestSchema(t)
	store, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	recipient := im.Recipient{AppID: "app-1", OpenID: "user-1"}
	checkNotification(t, store.EnableApprovalNotifications(ctx, recipient))
	token := seedApprovalNotifications(t, ctx, store)
	approvalID := requestNotificationApproval(t, ctx, store, token, "渠道竞争")
	delivery := claimNotification(t, ctx, store, recipient, approvalID, "pending", "")
	checkNotification(t, store.CompleteApprovalNotification(ctx, recipient, delivery, "message-1"))
	option := 1
	reply := im.Reply{ApprovalID: approvalID, OpenID: recipient.OpenID, MessageID: "message-1", Option: &option}
	invalidOption := 2
	for _, invalid := range []im.Reply{
		{ApprovalID: "different", OpenID: reply.OpenID, MessageID: reply.MessageID, Option: &option},
		{ApprovalID: approvalID, OpenID: "wrong-user", MessageID: reply.MessageID, Option: &option},
		{ApprovalID: approvalID, OpenID: reply.OpenID, MessageID: "wrong-message", Option: &option},
		{ApprovalID: approvalID, OpenID: reply.OpenID, MessageID: reply.MessageID, Option: &invalidOption},
		{OpenID: reply.OpenID, MessageID: reply.MessageID, Text: "  "},
	} {
		if _, err := store.AnswerApprovalFromIM(ctx, recipient, invalid); !errors.Is(err, im.ErrReplyUnavailable) {
			t.Fatalf("未拦截无效的渠道答复: %v", err)
		}
	}
	before, err := store.Workspace(ctx)
	checkNotification(t, err)
	start := make(chan struct{})
	var done sync.WaitGroup
	winners, failures := make([]workspace.Approval, 2), make([]error, 2)
	done.Add(2)
	go func() {
		defer done.Done()
		<-start
		winners[0], failures[0] = store.AnswerApproval(ctx, approvalID, "Web")
	}()
	go func() {
		defer done.Done()
		<-start
		winners[1], failures[1] = store.AnswerApprovalFromIM(ctx, recipient, reply)
	}()
	close(start)
	done.Wait()
	checkNotification(t, failures[0])
	checkNotification(t, failures[1])
	if winners[0].Answer != winners[1].Answer || (winners[0].Answer != "Web" && winners[0].Answer != "JSON") {
		t.Fatalf("两端答复结果不一致: %+v", winners)
	}
	after, err := store.Workspace(ctx)
	checkNotification(t, err)
	detail, err := store.TaskDetail(ctx, "root")
	checkNotification(t, err)
	if after.Cursor != before.Cursor+1 || len(detail.Runs) != 2 {
		t.Fatalf("并发答复推进了多个游标或运行: %d -> %d, runs=%d", before.Cursor, after.Cursor, len(detail.Runs))
	}

	store.Close()
	store, err = OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	newRecipient := im.Recipient{AppID: "app-2", OpenID: "user-2"}
	checkNotification(t, store.EnableApprovalNotifications(ctx, newRecipient))
	var channel string
	var appID, openID *string
	checkNotification(t, store.pool.QueryRow(ctx, `SELECT answered_via,answered_app_id,answered_open_id
		FROM muster_approvals WHERE id=$1`, approvalID).Scan(&channel, &appID, &openID))
	switch winners[0].Answer {
	case "Web":
		if channel != "web" || appID != nil || openID != nil {
			t.Fatalf("Web 首答人的身份不正确: %s, %v, %v", channel, appID, openID)
		}
	case "JSON":
		if channel != "feishu" || appID == nil || *appID != recipient.AppID || openID == nil || *openID != recipient.OpenID {
			t.Fatalf("飞书首答人的身份不正确: %s, %v, %v", channel, appID, openID)
		}
	}
	if empty, err := store.ClaimApprovalNotification(ctx, newRecipient); err != nil || empty.Lease != "" {
		t.Fatalf("新配置领取了旧应用的通知: %+v, %v", empty, err)
	}
	if _, err := store.AnswerApprovalFromIM(ctx, recipient, reply); !errors.Is(err, im.ErrReplyUnavailable) {
		t.Fatalf("旧配置仍可处理答复: %v", err)
	}
	resumed, err := store.ClaimRun(ctx, "home", token)
	checkNotification(t, err)
	checkNotification(t, store.RecordRunEvent(ctx, "home", token, resumed.ID, 1, workspace.RunEvent{
		Kind: "started", Generation: resumed.Generation, Lease: resumed.Lease,
	}))
	checkNotification(t, store.RecordRunEvent(ctx, "home", token, resumed.ID, 2, workspace.RunEvent{
		Kind: "approval_requested", Approval: &approval.Question{Question: "新配置的请示"},
		Generation: resumed.Generation, Lease: resumed.Lease,
	}))
	newID := "approval-" + stableUUID(resumed.ID)
	newDelivery := claimNotification(t, ctx, store, newRecipient, newID, "pending", "")
	if newDelivery.Approval.Question != "新配置的请示" {
		t.Fatalf("新配置未获得新生成的请示: %+v", newDelivery)
	}
	_, err = store.pool.Exec(ctx, `UPDATE muster_approval_notifications
		SET first_send_at=clock_timestamp()-interval '56 minutes',lease_until=clock_timestamp()-interval '1 second'
		WHERE approval_id=$1`, newID)
	checkNotification(t, err)
	if empty, err := store.ClaimApprovalNotification(ctx, newRecipient); err != nil || empty.Lease != "" {
		t.Fatalf("超过渠道去重窗口仍重新发送: %+v, %v", empty, err)
	}
}

func TestApprovalNotificationTracksOfflineResume(t *testing.T) {
	ctx, address := newTestSchema(t)
	store, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	recipient := im.Recipient{AppID: "app-1", OpenID: "user-1"}
	checkNotification(t, store.EnableApprovalNotifications(ctx, recipient))
	token := seedApprovalNotifications(t, ctx, store)
	approvalID := requestNotificationApproval(t, ctx, store, token, "转为 CSV？")
	const messageID = "message-1"
	initial := claimNotification(t, ctx, store, recipient, approvalID, "pending", "")
	checkNotification(t, store.CompleteApprovalNotification(ctx, recipient, initial, messageID))
	reply := im.Reply{ApprovalID: approvalID, OpenID: recipient.OpenID, MessageID: messageID, Text: "转为 CSV"}
	answered, err := store.AnswerApprovalFromIM(ctx, recipient, reply)
	checkNotification(t, err)
	if answered.Status != "answered" || !answered.WaitingDevice {
		t.Fatalf("离线答复未等待设备: %+v", answered)
	}
	_, err = store.AnswerApproval(ctx, approvalID, "改为 JSON")
	checkNotification(t, err)
	var channel, appID, openID string
	checkNotification(t, store.pool.QueryRow(ctx, `SELECT answered_via,answered_app_id,answered_open_id
		FROM muster_approvals WHERE id=$1`, approvalID).Scan(&channel, &appID, &openID))
	if channel != "feishu" || appID != recipient.AppID || openID != recipient.OpenID {
		t.Fatal("重复的 Web 答复覆盖了飞书首答人")
	}

	assertWaiting := func(want bool) int64 {
		t.Helper()
		snapshot, err := store.Workspace(ctx)
		checkNotification(t, err)
		detail, err := store.TaskDetail(ctx, answered.TaskID)
		checkNotification(t, err)
		if snapshot.Approvals[0].WaitingDevice != want || detail.Approvals[0].WaitingDevice != want {
			t.Fatalf("工作区与任务详情的设备状态不正确: %+v, %+v", snapshot.Approvals, detail.Approvals)
		}
		return snapshot.Cursor
	}
	revision := assertWaiting(true)
	offline := claimNotification(t, ctx, store, recipient, approvalID, "answered", messageID)
	if !offline.Approval.WaitingDevice {
		t.Fatal("离线更新未标记等待设备")
	}
	checkNotification(t, store.CompleteApprovalNotification(ctx, recipient, offline, messageID))
	empty, err := store.ClaimApprovalNotification(ctx, recipient)
	checkNotification(t, err)
	if empty.Lease != "" {
		t.Fatal("已确认的等待状态被重复领取")
	}
	checkNotification(t, store.Heartbeat(ctx, "home", token))
	if got := assertWaiting(false); got != revision {
		t.Fatalf("仅设备心跳变化时更新了工作区游标: %d -> %d", revision, got)
	}
	online := claimNotification(t, ctx, store, recipient, approvalID, "answered", messageID)
	if online.Approval.WaitingDevice {
		t.Fatal("上线更新仍标记等待设备")
	}
	checkNotification(t, store.RetryApprovalNotification(ctx, recipient, online))
	store.Close()
	store, err = OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	checkNotification(t, store.EnableApprovalNotifications(ctx, recipient))
	// 上线卡片的响应可能已丢失。即使设备重新离线，也须再次更新原卡片。
	_, err = store.pool.Exec(ctx, `UPDATE muster_devices SET last_seen_at=statement_timestamp()-interval '91 seconds' WHERE device_id='home';
		UPDATE muster_approval_notifications SET next_attempt_at=statement_timestamp()`)
	checkNotification(t, err)
	assertWaiting(true)
	retried := claimNotification(t, ctx, store, recipient, approvalID, "answered", messageID)
	if !retried.Approval.WaitingDevice || retried.Lease == online.Lease {
		t.Fatalf("离线重试没有重新生成卡片快照: %+v", retried)
	}
	checkNotification(t, store.CompleteApprovalNotification(ctx, recipient, retried, messageID))
	checkNotification(t, store.Heartbeat(ctx, "home", token))
	resume, err := store.ClaimRun(ctx, "home", token)
	checkNotification(t, err)
	if resume.SessionID != "native" {
		t.Fatalf("设备上线后未续接原生会话: %+v", resume)
	}
	checkNotification(t, store.RecordRunEvent(ctx, "home", token, resume.ID, 1, workspace.RunEvent{
		Kind: "completed", Text: "已处理", Generation: resume.Generation, Lease: resume.Lease,
	}))
	assertWaiting(false)
	handled := claimNotification(t, ctx, store, recipient, approvalID, "handled", messageID)
	if handled.Approval.WaitingDevice {
		t.Fatal("已处理卡片仍标记等待设备")
	}
	checkNotification(t, store.CompleteApprovalNotification(ctx, recipient, handled, messageID))
}

func TestApprovalResponderMigrationPreservesUnknownOrigins(t *testing.T) {
	ctx, address := newTestSchema(t)
	store, err := OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	token := seedApprovalNotifications(t, ctx, store)
	id := requestNotificationApproval(t, ctx, store, token, "旧版本问题")
	_, err = store.AnswerApproval(ctx, id, "旧答复")
	checkNotification(t, err)
	_, err = store.pool.Exec(ctx, `ALTER TABLE muster_approvals DROP COLUMN answered_via, DROP COLUMN answered_app_id, DROP COLUMN answered_open_id`)
	checkNotification(t, err)
	store.Close()
	store, err = OpenInventory(ctx, address)
	checkNotification(t, err)
	t.Cleanup(store.Close)
	got, err := store.AnswerApproval(ctx, id, "迟到的答复")
	checkNotification(t, err)
	if got.Answer != "旧答复" {
		t.Fatalf("旧答复被覆盖: %+v", got)
	}
	var channel, appID, openID *string
	checkNotification(t, store.pool.QueryRow(ctx, `SELECT answered_via,answered_app_id,answered_open_id
		FROM muster_approvals WHERE id=$1`, id).Scan(&channel, &appID, &openID))
	if channel != nil || appID != nil || openID != nil {
		t.Fatal("旧答复被补入未经核验的来源")
	}
}

func seedApprovalNotifications(t *testing.T, ctx context.Context, store *InventoryStore) [32]byte {
	t.Helper()
	var token [32]byte
	token[0] = 1
	_, err := store.pool.Exec(ctx, `INSERT INTO muster_devices(device_id,token_hash,name,hostname,os,arch)
		VALUES ('home',$1,'Home','home','windows','amd64')`, token[:])
	checkNotification(t, err)
	_, err = store.pool.Exec(ctx, `
		INSERT INTO muster_directories(device_id,directory_id,label) VALUES ('home','folder','Folder');
		INSERT INTO muster_projects(id,name,device_id,directory_id,directory_label)
			VALUES ('project','Project','home','folder','Folder');
		INSERT INTO muster_project_bindings(id,project_id,device_id,directory_id,agent_kind,is_default)
			VALUES ('project','project','home','folder','dsh',true);
		INSERT INTO muster_tasks(id,project_id,project_name,device_id,directory_id,title,status,current_run_id,root_id,binding_id)
			VALUES ('root','project','Project','home','folder','Root','queued','root-run','root','project');
		INSERT INTO muster_runs(id,task_id,input,device_id) VALUES ('root-run','root','input','home');
	`, pgx.QueryExecModeSimpleProtocol)
	checkNotification(t, err)
	return token
}

func requestNotificationApproval(t *testing.T, ctx context.Context, store *InventoryStore, token [32]byte, question string) string {
	t.Helper()
	run, err := store.ClaimRun(ctx, "home", token)
	checkNotification(t, err)
	checkNotification(t, store.RecordRunEvent(ctx, "home", token, run.ID, 1, workspace.RunEvent{
		Kind: "started", SessionID: "native", Generation: run.Generation, Lease: run.Lease,
	}))
	checkNotification(t, store.RecordRunEvent(ctx, "home", token, run.ID, 2, workspace.RunEvent{
		Kind: "approval_requested", Generation: run.Generation, Lease: run.Lease,
		Approval: &approval.Question{Question: question, Options: []string{"CSV", "JSON"}},
	}))
	return "approval-" + stableUUID(run.ID)
}

func claimNotification(t *testing.T, ctx context.Context, store *InventoryStore, recipient im.Recipient, id, status, message string) im.Delivery {
	t.Helper()
	value, err := store.ClaimApprovalNotification(ctx, recipient)
	checkNotification(t, err)
	if value.Lease == "" || value.Approval.ID != id || value.Approval.Status != status || value.MessageID != message {
		t.Fatalf("通知快照与预期不符: %+v, id=%s state=%s message=%s", value, id, status, message)
	}
	return value
}

func checkNotification(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
