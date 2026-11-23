package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/hub/im"
	"github.com/baiyucraft/agent-muster/internal/hub/postgres"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

func TestApprovalRestartsAcrossCommitAndAcknowledgement(t *testing.T) {
	ctx, conn, schema, address := newWorkspaceTestSchema(t)
	recipient := im.Recipient{AppID: "approval-restart-app", OpenID: "approval-restart-user"}
	var store *postgres.InventoryStore
	var server *httptest.Server
	var do workspaceRequest
	restartHub := func() {
		t.Helper()
		if server != nil {
			server.Close()
			store.Close()
		}
		var err error
		store, err = postgres.OpenInventory(ctx, address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(store.Close)
		if err := store.EnableApprovalNotifications(ctx, recipient); err != nil {
			t.Fatal(err)
		}
		server, do = serveWorkspaceFixture(t, ctx, store)
	}
	restartHub()

	var pairing struct{ Code string }
	do("POST", "pairing-codes", testAdmin, map[string]string{"name": "审批恢复设备"}, 201, &pairing)
	var device struct {
		ID    string `json:"device_id"`
		Token string
	}
	do("POST", "devices/enroll", "", map[string]string{
		"code": pairing.Code, "hostname": "approval-device", "os": "windows", "arch": "amd64",
	}, 201, &device)
	prefix := "devices/" + device.ID + "/"
	dir := "dir-" + strings.Repeat("a", 32)
	do("PUT", prefix+"directories", device.Token, map[string]any{
		"directories": []workspace.Directory{{ID: dir, Label: "素材"}},
	}, 204, nil)
	const projectID = "40000000-0000-4000-8000-000000000001"
	const messageID = "40000000-0000-4000-8000-000000000002"
	do("PUT", "projects/"+projectID, testAdmin, map[string]string{
		"name": "素材索引", "device_id": device.ID, "directory_id": dir,
	}, 200, nil)
	do("POST", "conversation/messages", testAdmin, map[string]string{"id": messageID, "text": "生成索引"}, 202, nil)
	var delivery workspace.Delivery
	do("POST", "plugin/deliveries/claim", testPlugin, nil, 200, &delivery)
	deliveryPath := "plugin/deliveries/" + delivery.ID
	do("POST", deliveryPath+"/dispatch", testPlugin, workspace.Dispatch{
		Lease: delivery.Lease, ProjectID: projectID, Title: "生成索引", Prompt: "生成索引",
	}, 200, nil)
	do("PUT", deliveryPath+"/complete", testPlugin, workspace.Completion{Lease: delivery.Lease, Status: "handled"}, 204, nil)
	var run workspace.RunClaim
	do("POST", prefix+"runs/claim", device.Token, nil, 200, &run)
	const native = "approval-restart-session"
	events := prefix + "runs/" + run.ID + "/events/"
	do("PUT", events+"1", device.Token, workspace.RunEvent{
		Kind: "started", SessionID: native, Generation: run.Generation, Lease: run.Lease,
	}, 204, nil)
	question := workspace.RunEvent{
		Kind: "approval_requested", Generation: run.Generation, Lease: run.Lease,
		Approval: &approval.Question{Question: "使用 CSV 吗？", Options: []string{"CSV", "JSON"}},
	}
	snapshot := func() workspace.Snapshot {
		t.Helper()
		var value workspace.Snapshot
		do("GET", "workspace", testAdmin, nil, 200, &value)
		return value
	}
	assertEventCount := func(want int) {
		t.Helper()
		var count int
		err := conn.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.muster_run_events WHERE run_id=$1 AND sequence=2`, run.ID).Scan(&count)
		if err != nil || count != want {
			t.Fatalf("请示运行事件有 %d 条，预期 %d，%v", count, want, err)
		}
	}
	before := snapshot()

	// 让最后一次游标事件写入失败。此时运行、主对话消息、请示和 IM 待办已在同一事务内写入。
	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %[1]s.reject_approval_revision() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'test failure before approval commit'; END $$;
		CREATE TRIGGER reject_approval_revision BEFORE INSERT ON %[1]s.muster_workspace_events
		FOR EACH ROW EXECUTE FUNCTION %[1]s.reject_approval_revision();`, schema), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	do("PUT", events+"2", device.Token, question, 503, nil)
	restartHub()
	afterRollback := snapshot()
	if afterRollback.Cursor != before.Cursor || len(afterRollback.Approvals) != 0 || len(afterRollback.Messages) != len(before.Messages) ||
		len(afterRollback.Tasks) != 1 || afterRollback.Tasks[0].Status != "running" {
		t.Fatalf("重启后存在未提交的请示变更 %+v", afterRollback)
	}
	var interrupted workspace.Detail
	do("GET", "tasks/"+run.TaskID, testAdmin, nil, 200, &interrupted)
	if len(interrupted.Runs) != 1 || interrupted.Runs[0].ID != run.ID || interrupted.Runs[0].Status != "running" {
		t.Fatalf("请示事务回滚后运行状态错误 %+v", interrupted.Runs)
	}
	assertEventCount(0)
	if _, err := conn.Exec(ctx, "DROP TRIGGER reject_approval_revision ON "+schema+".muster_workspace_events"); err != nil {
		t.Fatal(err)
	}
	if missing, err := store.ClaimApprovalNotification(ctx, recipient); err != nil || missing.Lease != "" {
		t.Fatalf("回滚后仍有待发送卡片 %+v，%v", missing, err)
	}

	// 真实处理器成功提交并生成 204 后，测试服务关闭响应连接。发送端只能按原回执重传。
	handler := NewHandler(store, testAdmin, nil, WorkspaceOptions{Store: store, PluginToken: testPlugin})
	processed := make(chan int, 1)
	withoutAck := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		processed <- response.Code
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(withoutAck.Close)
	payload, err := json.Marshal(question)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, "PUT", withoutAck.URL+"/api/v1/"+events+"2", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+device.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := withoutAck.Client().Do(request)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("设备未遇到预期的 HTTP 确认丢失，%v", err)
	}
	select {
	case status := <-processed:
		if status != http.StatusNoContent {
			t.Fatalf("截断响应前处理器返回 HTTP %d", status)
		}
	default:
		t.Fatal("响应中断发生在真实处理器完成前")
	}
	withoutAck.Close()

	// 尚未领取首张卡片即重启。重传前先核对已提交的请示和看板状态。
	// 启动时会补建缺失的通知，重启前直接确认原事务已经写入待办。
	var pendingNotifications int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.muster_approval_notifications n
		JOIN `+schema+`.muster_approvals a ON a.id=n.approval_id
		WHERE a.run_id=$1 AND n.app_id=$2 AND n.open_id=$3 AND n.desired_status='pending'`,
		run.ID, recipient.AppID, recipient.OpenID).Scan(&pendingNotifications)
	if err != nil || pendingNotifications != 1 {
		t.Fatalf("重启前已提交的待发送卡片有 %d 条，预期 1，%v", pendingNotifications, err)
	}
	restartHub()
	committed := snapshot()
	if committed.Cursor != before.Cursor+1 || len(committed.Approvals) != 1 || len(committed.Messages) != len(before.Messages)+1 ||
		len(committed.Tasks) != 1 || committed.Tasks[0].Status != "waiting_reply" {
		t.Fatalf("重启后未恢复已提交的请示 %+v", committed)
	}
	pending := committed.Approvals[0]
	message := committed.Messages[len(committed.Messages)-1]
	if pending.Status != "pending" || pending.RunID != run.ID || pending.Question != question.Approval.Question ||
		message.ID != pending.ID || message.TaskID != run.TaskID || message.Kind != "approval" {
		t.Fatalf("恢复的请示和主对话消息不一致 %+v，%+v", pending, message)
	}
	do("PUT", events+"2", device.Token, question, 204, nil)
	assertEventCount(1)
	if replayed := snapshot(); replayed.Cursor != committed.Cursor || len(replayed.Approvals) != 1 || len(replayed.Messages) != len(committed.Messages) {
		t.Fatalf("相同请示重传产生额外写入 %+v", replayed)
	}
	assertWorkspaceReplay(t, ctx, server, before.Cursor, committed.Cursor)
	card, err := store.ClaimApprovalNotification(ctx, recipient)
	if err != nil || card.Lease == "" || card.MessageID != "" || card.Approval.ID != pending.ID || card.Approval.Status != "pending" {
		t.Fatalf("首次 IM 卡片未在重启后恢复 %+v，%v", card, err)
	}
	if err := store.CompleteApprovalNotification(ctx, recipient, card, "approval-restart-card"); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := store.ClaimApprovalNotification(ctx, recipient); err != nil || duplicate.Lease != "" {
		t.Fatalf("同一请示有多条待发送卡片 %+v，%v", duplicate, err)
	}
}
