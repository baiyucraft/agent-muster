package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestApprovalAnswerResumesBeforeAppendedWork(t *testing.T) {
	ctx, _, _, server, do := openWorkspaceFixture(t)
	var pairing struct{ Code string }
	do("POST", "pairing-codes", testAdmin, map[string]string{"name": "Test device"}, 201, &pairing)
	var device struct {
		ID    string `json:"device_id"`
		Token string
	}
	do("POST", "devices/enroll", "", map[string]string{
		"code": pairing.Code, "hostname": "test", "os": "windows", "arch": "amd64",
	}, 201, &device)
	prefix := "devices/" + device.ID + "/"
	dir := "dir-" + strings.Repeat("a", 32)
	do("PUT", prefix+"directories", device.Token, map[string]any{
		"directories": []workspace.Directory{{ID: dir, Label: "临时目录"}},
	}, 204, nil)
	const project = "20000000-0000-4000-8000-000000000001"
	do("PUT", "projects/"+project, testAdmin, map[string]string{
		"name": "素材索引", "device_id": device.ID, "directory_id": dir,
	}, 200, nil)

	dispatch := func(messageID, taskID, prompt string) {
		t.Helper()
		do("POST", "conversation/messages", testAdmin, map[string]string{"id": messageID, "text": prompt, "task_id": taskID}, 202, nil)
		var delivery workspace.Delivery
		do("POST", "plugin/deliveries/claim", testPlugin, nil, 200, &delivery)
		input := workspace.Dispatch{TaskID: taskID, Lease: delivery.Lease, Prompt: prompt}
		if taskID == "" {
			input.ProjectID, input.Title = project, "整理素材"
		}
		do("POST", "plugin/deliveries/"+delivery.ID+"/dispatch", testPlugin, input, 200, nil)
		do("PUT", "plugin/deliveries/"+delivery.ID+"/complete", testPlugin,
			workspace.Completion{Lease: delivery.Lease, Status: "handled"}, 204, nil)
	}
	record := func(run workspace.RunClaim, sequence int, event workspace.RunEvent, code int) {
		t.Helper()
		event.Generation, event.Lease = run.Generation, run.Lease
		do("PUT", fmt.Sprintf("%sruns/%s/events/%d", prefix, run.ID, sequence), device.Token, event, code, nil)
	}
	snapshot := func() workspace.Snapshot {
		t.Helper()
		var result workspace.Snapshot
		do("GET", "workspace", testAdmin, nil, 200, &result)
		return result
	}

	dispatch("20000000-0000-4000-8000-000000000002", "", "生成索引")
	var source workspace.RunClaim
	do("POST", prefix+"runs/claim", device.Token, nil, 200, &source)
	if !source.ApprovalSupported {
		t.Fatal("领取结果缺少请示能力")
	}
	const nativeSession = "native-approval"
	record(source, 1, workspace.RunEvent{Kind: "started", SessionID: nativeSession}, 204)
	const firstAppend = "20000000-0000-4000-8000-000000000003"
	dispatch(firstAppend, source.TaskID, "补充文件大小")
	asked := workspace.RunEvent{Kind: "approval_requested", Approval: &approval.Question{Question: "采用 CSV 吗？"}}
	invalid := asked
	invalid.Approval = &approval.Question{Question: " "}
	record(source, 2, invalid, 400)
	record(source, 2, asked, 204)
	beforeAnswer := snapshot()
	record(source, 2, asked, 204)
	if afterReplay := snapshot(); afterReplay.Cursor != beforeAnswer.Cursor {
		t.Fatalf("请示重传增加游标: %d != %d", afterReplay.Cursor, beforeAnswer.Cursor)
	}
	if len(beforeAnswer.Approvals) != 1 || beforeAnswer.Approvals[0].Status != "pending" ||
		beforeAnswer.Approvals[0].Options == nil || beforeAnswer.Tasks[0].Status != "waiting_reply" ||
		beforeAnswer.Tasks[0].ReportPending {
		t.Fatalf("请示后快照不正确: %+v", beforeAnswer)
	}
	pending := beforeAnswer.Approvals[0]
	if last := beforeAnswer.Messages[len(beforeAnswer.Messages)-1]; last.ID != pending.ID ||
		last.Kind != "approval" || last.Text != pending.Question || last.TaskID != source.TaskID {
		t.Fatalf("主对话缺少请示: %+v", last)
	}
	const secondAppend = "20000000-0000-4000-8000-000000000004"
	dispatch(secondAppend, source.TaskID, "补充修改时间")
	do("POST", prefix+"runs/claim", device.Token, nil, 204, nil)
	do("POST", "plugin/deliveries/claim", testPlugin, nil, 204, nil)
	path := "approvals/" + pending.ID + "/answer"
	do("POST", path, device.Token, map[string]string{"answer": "CSV"}, 401, nil)
	do("POST", path, testPlugin, map[string]string{"answer": "CSV"}, 401, nil)
	do("POST", path, testAdmin, map[string]string{"answer": " "}, 400, nil)

	cursor := snapshot().Cursor
	type submission struct {
		value workspace.Approval
		err   error
	}
	results := make(chan submission, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for _, answer := range []string{"CSV", "JSON"} {
		go func() {
			body, err := json.Marshal(map[string]string{"answer": answer})
			ready.Done()
			<-start
			var result submission
			if err != nil {
				result.err = err
				results <- result
				return
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/"+path, bytes.NewReader(body))
			if err == nil {
				request.Header.Set("Authorization", "Bearer "+testAdmin)
				request.Header.Set("Content-Type", "application/json")
				response, requestErr := server.Client().Do(request)
				err = requestErr
				if err == nil {
					defer response.Body.Close()
					if response.StatusCode != http.StatusOK {
						err = fmt.Errorf("答复状态码 %d", response.StatusCode)
					} else {
						err = json.NewDecoder(response.Body).Decode(&result.value)
					}
				}
			}
			result.err = err
			results <- result
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.value.Answer != second.value.Answer ||
		first.value.Status != "answered" || second.value.Status != "answered" || first.value.AnsweredAt == nil {
		t.Fatalf("并发答复未返回同一结果: %+v, %+v", first, second)
	}
	if got := snapshot(); got.Cursor != cursor+1 || got.Tasks[0].Status != "queued" || got.Tasks[0].ReportPending {
		t.Fatalf("答复后快照不正确: %+v", got)
	}

	var resumed workspace.RunClaim
	do("POST", prefix+"runs/claim", device.Token, nil, 200, &resumed)
	if resumed.ID == source.ID || resumed.ID == "run-"+firstAppend || resumed.ID == "run-"+secondAppend ||
		resumed.SessionID != nativeSession || resumed.DirectoryID != dir || !strings.Contains(resumed.Prompt, first.value.Answer) {
		t.Fatalf("未先续接原会话处理答复: %+v", resumed)
	}
	record(resumed, 1, workspace.RunEvent{Kind: "completed", Text: "已处理答复"}, 204)
	do("POST", path, testAdmin, map[string]string{"answer": "迟到答复"}, 200, &pending)
	if pending.Status != "handled" || pending.HandledAt == nil || pending.Answer != first.value.Answer {
		t.Fatalf("答复处理状态不正确: %+v", pending)
	}
	for _, appendID := range []string{firstAppend, secondAppend} {
		var next workspace.RunClaim
		do("POST", prefix+"runs/claim", device.Token, nil, 200, &next)
		if next.ID != "run-"+appendID || next.SessionID != nativeSession {
			t.Fatalf("答复后追加轮次顺序不正确: %+v", next)
		}
		record(next, 1, workspace.RunEvent{Kind: "completed", Text: "已完成"}, 204)
	}
	var detail workspace.Detail
	do("GET", "tasks/"+source.TaskID, testAdmin, nil, 200, &detail)
	if len(detail.Runs) != 4 || len(detail.Approvals) != 1 || detail.Task.Status != "completed" ||
		detail.Approvals[0].Status != "handled" || !detail.Task.ReportPending {
		t.Fatalf("任务终态或轮次不正确: %+v", detail)
	}
	for _, input := range detail.Inputs {
		if input.Kind == "approval" && input.RunID == resumed.ID && input.ID == pending.ID {
			return
		}
	}
	t.Fatalf("续接运行缺少请示输入引用: %+v", detail.Inputs)
}
