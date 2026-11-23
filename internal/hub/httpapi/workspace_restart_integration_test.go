package httpapi

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/hub/im"
	"github.com/baiyucraft/agent-muster/internal/hub/postgres"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestWorkspaceRecoversWorkAcceptedWhilePluginOffline(t *testing.T) {
	ctx, _, _, address := newWorkspaceTestSchema(t)
	recipient := im.Recipient{AppID: "integration-app", OpenID: "integration-user"}
	startHub := func() (*postgres.InventoryStore, *httptest.Server, workspaceRequest) {
		t.Helper()
		store, err := postgres.OpenInventory(ctx, address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(store.Close)
		if err := store.EnableApprovalNotifications(ctx, recipient); err != nil {
			t.Fatal(err)
		}
		server, do := serveWorkspaceFixture(t, ctx, store)
		return store, server, do
	}
	store, server, do := startHub()

	type device struct {
		ID    string `json:"device_id"`
		Token string
	}
	enroll := func(label, dir, project string) device {
		t.Helper()
		var pairing struct{ Code string }
		do("POST", "pairing-codes", testAdmin, map[string]string{"name": label}, 201, &pairing)
		var identity device
		do("POST", "devices/enroll", "", map[string]string{
			"code": pairing.Code, "hostname": label, "os": "windows", "arch": "amd64",
		}, 201, &identity)
		do("PUT", "devices/"+identity.ID+"/directories", identity.Token, map[string]any{
			"directories": []workspace.Directory{{ID: dir, Label: label}},
		}, 204, nil)
		do("PUT", "projects/"+project, testAdmin, map[string]string{
			"name": label, "device_id": identity.ID, "directory_id": dir,
		}, 200, nil)
		return identity
	}
	post := func(id, text string) workspace.Message {
		t.Helper()
		var message workspace.Message
		do("POST", "conversation/messages", testAdmin, map[string]string{"id": id, "text": text}, 202, &message)
		return message
	}
	claimDelivery := func(kind string) workspace.Delivery {
		t.Helper()
		var delivery workspace.Delivery
		do("POST", "plugin/deliveries/claim", testPlugin, nil, 200, &delivery)
		if delivery.Kind != kind {
			t.Fatalf("领取 %s 投递时收到 %+v", kind, delivery)
		}
		return delivery
	}
	completeDelivery := func(delivery workspace.Delivery, text string) {
		t.Helper()
		payload := workspace.Completion{Lease: delivery.Lease, Status: "handled", Text: text}
		do("PUT", "plugin/deliveries/"+delivery.ID+"/complete", testPlugin, payload, 204, nil)
	}
	dispatch := func(message workspace.Message, project string) string {
		t.Helper()
		delivery := claimDelivery("message")
		if delivery.MessageID != message.ID || delivery.Text != message.Text {
			t.Fatalf("领取到错误的用户消息 %+v", delivery)
		}
		var assigned struct {
			TaskID string `json:"task_id"`
		}
		do("POST", "plugin/deliveries/"+delivery.ID+"/dispatch", testPlugin, workspace.Dispatch{
			Lease: delivery.Lease, ProjectID: project, Title: message.Text, Prompt: message.Text,
		}, 200, &assigned)
		completeDelivery(delivery, "")
		return assigned.TaskID
	}
	claimRun := func(identity device, taskID string) workspace.RunClaim {
		t.Helper()
		var run workspace.RunClaim
		do("POST", "devices/"+identity.ID+"/runs/claim", identity.Token, nil, 200, &run)
		if run.TaskID != taskID {
			t.Fatalf("设备领取了错误的任务 %+v", run)
		}
		return run
	}
	record := func(identity device, run workspace.RunClaim, sequence int, event workspace.RunEvent) {
		t.Helper()
		event.Generation, event.Lease = run.Generation, run.Lease
		path := fmt.Sprintf("devices/%s/runs/%s/events/%d", identity.ID, run.ID, sequence)
		do("PUT", path, identity.Token, event, 204, nil)
	}
	snapshot := func() workspace.Snapshot {
		t.Helper()
		var snapshot workspace.Snapshot
		do("GET", "workspace", testAdmin, nil, 200, &snapshot)
		return snapshot
	}
	assertTask := func(taskID, status, result string, reportPending bool) workspace.Detail {
		t.Helper()
		var detail workspace.Detail
		do("GET", "tasks/"+taskID, testAdmin, nil, 200, &detail)
		if detail.Task.Status != status || detail.Task.Result != result || detail.Task.ReportPending != reportPending {
			t.Fatalf("任务 %s 状态异常 %+v", taskID, detail.Task)
		}
		return detail
	}

	const projectA = "30000000-0000-4000-8000-000000000001"
	const projectB = "30000000-0000-4000-8000-000000000002"
	dirA, dirB := "dir-"+strings.Repeat("a", 32), "dir-"+strings.Repeat("b", 32)
	deviceA, deviceB := enroll("设备 A", dirA, projectA), enroll("设备 B", dirB, projectB)
	taskA := dispatch(post("30000000-0000-4000-8000-000000000003", "整理文件"), projectA)
	taskB := dispatch(post("30000000-0000-4000-8000-000000000004", "生成索引"), projectB)
	runA, runB := claimRun(deviceA, taskA), claimRun(deviceB, taskB)
	record(deviceA, runA, 1, workspace.RunEvent{Kind: "started", SessionID: "native-a"})
	const nativeB = "native-b"
	record(deviceB, runB, 1, workspace.RunEvent{Kind: "started", SessionID: nativeB})
	record(deviceB, runB, 2, workspace.RunEvent{Kind: "approval_requested", Approval: &approval.Question{
		Question: "索引使用哪种格式？", Options: []string{"CSV", "JSON"},
	}})
	assertTask(taskA, "running", "", false)
	assertTask(taskB, "waiting_reply", "", false)
	before := snapshot()
	if len(before.Approvals) != 1 || before.Approvals[0].TaskID != taskB || before.Approvals[0].Status != "pending" {
		t.Fatalf("设备 B 缺少待答复的请示 %+v", before.Approvals)
	}
	approvalID := before.Approvals[0].ID
	card, err := store.ClaimApprovalNotification(ctx, recipient)
	if err != nil || card.Lease == "" || card.Approval.ID != approvalID {
		t.Fatalf("领取 IM 卡片失败 %+v，%v", card, err)
	}
	const cardID = "integration-im-card"
	if err := store.CompleteApprovalNotification(ctx, recipient, card, cardID); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceReplay(t, ctx, server, before.Cursor-1, before.Cursor)

	// 插件停止轮询。Web、设备和 IM 的入口继续写入同一个工作区。
	messageC := post("30000000-0000-4000-8000-000000000005", "核对备注")
	if messageC.Status != "accepted" || messageC.TaskID != "" {
		t.Fatalf("插件停机时 Web 未即时受理消息 %+v", messageC)
	}
	const resultA = "文件已整理"
	endA := workspace.RunEvent{Kind: "completed", Text: resultA}
	record(deviceA, runA, 2, endA)
	choice := 0
	imReply := im.Reply{ApprovalID: approvalID, OpenID: recipient.OpenID, MessageID: cardID, Option: &choice}
	answer, err := store.AnswerApprovalFromIM(ctx, recipient, imReply)
	if err != nil || answer.Answer != "CSV" || answer.Status != "answered" || answer.AnsweredAt == nil {
		t.Fatalf("插件停机时 IM 未保存答复 %+v，%v", answer, err)
	}
	assertTask(taskA, "completed", resultA, true)
	assertTask(taskB, "queued", "", false)
	offline := snapshot()
	if offline.Cursor != before.Cursor+3 {
		t.Fatalf("停机期间三项写入的游标增量为 %d", offline.Cursor-before.Cursor)
	}

	server.Close()
	store.Close()
	store, server, do = startHub()
	assertWorkspaceReplay(t, ctx, server, before.Cursor, offline.Cursor)
	assertTask(taskA, "completed", resultA, true)
	assertTask(taskB, "queued", "", false)
	record(deviceA, runA, 2, endA)
	answerReplay, err := store.AnswerApprovalFromIM(ctx, recipient, imReply)
	if err != nil || answerReplay.Status != "answered" || answerReplay.AnsweredAt == nil || !answerReplay.AnsweredAt.Equal(*answer.AnsweredAt) {
		t.Fatalf("重启后重复答复改变记录 %+v，%v", answerReplay, err)
	}
	update, err := store.ClaimApprovalNotification(ctx, recipient)
	if err != nil || update.Lease == "" || update.MessageID != cardID || update.Approval.ID != approvalID || update.Approval.Status != "answered" {
		t.Fatalf("重启后未恢复对原 IM 卡片的更新 %+v，%v", update, err)
	}
	if err := store.CompleteApprovalNotification(ctx, recipient, update, cardID); err != nil {
		t.Fatal(err)
	}
	if current := snapshot(); current.Cursor != offline.Cursor || len(current.Tasks) != 2 {
		t.Fatalf("重启及回执重传改变了工作区 %+v", current)
	}

	// 插件恢复后受理停机期间的消息，设备 B 用原生会话处理已保存的答复。
	taskC := dispatch(messageC, projectA)
	resumed := claimRun(deviceB, taskB)
	if resumed.ID == runB.ID || resumed.Generation <= runB.Generation || resumed.SessionID != nativeB ||
		resumed.DirectoryID != dirB || !strings.Contains(resumed.Prompt, `"answer":"CSV"`) {
		t.Fatalf("请示答复未使用设备 B 的原生会话续接 %+v", resumed)
	}
	record(deviceB, resumed, 1, workspace.RunEvent{Kind: "started", SessionID: nativeB})
	record(deviceB, resumed, 2, workspace.RunEvent{Kind: "completed", Text: "已生成 CSV 索引"})
	do("POST", "devices/"+deviceB.ID+"/runs/claim", deviceB.Token, nil, 204, nil)
	done := assertTask(taskB, "completed", "已生成 CSV 索引", true)
	if len(done.Runs) != 2 || len(done.Approvals) != 1 || done.Approvals[0].Status != "handled" || done.Approvals[0].Answer != "CSV" {
		t.Fatalf("请示答复未恰好执行一次 %+v", done)
	}
	runC := claimRun(deviceA, taskC)
	record(deviceA, runC, 1, workspace.RunEvent{Kind: "started", SessionID: "native-c"})
	record(deviceA, runC, 2, workspace.RunEvent{Kind: "completed", Text: "已核对备注"})

	reports := map[string]string{taskA: resultA, taskB: "已生成 CSV 索引", taskC: "已核对备注"}
	for range len(reports) {
		delivery := claimDelivery("report")
		result, ok := reports[delivery.TaskID]
		if !ok || !strings.Contains(delivery.Text, result) {
			t.Fatalf("出现意外的最终汇报 %+v", delivery)
		}
		completeDelivery(delivery, result)
		completeDelivery(delivery, result)
	}
	do("POST", "plugin/deliveries/claim", testPlugin, nil, 204, nil)
	counts := make(map[string]int)
	for _, message := range snapshot().Messages {
		if message.Kind == "report" {
			counts[message.TaskID]++
		}
	}
	for taskID, result := range reports {
		assertTask(taskID, "completed", result, false)
		if counts[taskID] != 1 {
			t.Fatalf("根任务 %s 在主对话中出现 %d 条最终汇报", taskID, counts[taskID])
		}
	}
}

func assertWorkspaceReplay(t *testing.T, ctx context.Context, server *httptest.Server, cursor, last int64) {
	t.Helper()
	streamContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(streamContext, "GET", fmt.Sprintf("%s/api/v1/events?after=%d", server.URL, cursor), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testAdmin)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE 重连失败，HTTP %d", response.StatusCode)
	}
	lines := bufio.NewScanner(response.Body)
	want := cursor + 1
	for lines.Scan() {
		if !strings.HasPrefix(lines.Text(), "id: ") {
			continue
		}
		if lines.Text() != fmt.Sprintf("id: %d", want) {
			t.Fatalf("SSE 重连后收到 %q，预期游标 %d", lines.Text(), want)
		}
		if want == last {
			return
		}
		want++
	}
	t.Fatalf("SSE 未补读游标 %d 至 %d，%v", want, last, lines.Err())
}
