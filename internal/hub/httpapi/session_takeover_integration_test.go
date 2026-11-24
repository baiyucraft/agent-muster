package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestSessionTakeoverCreatesIndependentRunWithoutPublishPermission(t *testing.T) {
	for _, agent := range []struct{ kind, sessionID string }{
		{kind: "dsh", sessionID: "native-test"},
		{kind: "codex", sessionID: "20000000-0000-4000-8000-000000000004"},
	} {
		t.Run(agent.kind, func(t *testing.T) {
			testSessionTakeover(t, agent.kind, agent.sessionID)
		})
	}
}

func testSessionTakeover(t *testing.T, agentKind, sessionID string) {
	t.Helper()
	ctx, _, _, server, do := openWorkspaceFixture(t)
	var pairing struct{ Code string }
	do("POST", "pairing-codes", testAdmin, map[string]string{"name": "开发设备"}, 201, &pairing)
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
		"directories": []workspace.Directory{{ID: dir, Label: "开发目录"}},
	}, 204, nil)
	const project = "20000000-0000-4000-8000-000000000001"
	policy := gitdelivery.Policy{
		BaseRef: "refs/heads/main", Remote: "origin",
		Test: gitdelivery.Command{Program: "go", Args: []string{"test", "./..."}, TimeoutSeconds: 30},
	}
	do("PUT", "projects/"+project, testAdmin, map[string]any{
		"name": "测试项目", "device_id": device.ID, "directory_id": dir, "agent_kind": agentKind, "git_policy": policy,
	}, 200, nil)
	do("POST", "conversation/messages", testAdmin, map[string]string{
		"id": "20000000-0000-4000-8000-000000000002", "text": "修复并推送",
	}, 202, nil)
	var delivery workspace.Delivery
	do("POST", "plugin/deliveries/claim", testPlugin, nil, 200, &delivery)
	deliveryPath := "plugin/deliveries/" + delivery.ID
	do("POST", deliveryPath+"/dispatch", testPlugin, workspace.Dispatch{
		Lease: delivery.Lease, ProjectID: project, Title: "修复并推送", Prompt: "修复并推送",
		GitDelivery: true, GitPublish: true,
	}, 200, nil)
	do("PUT", deliveryPath+"/complete", testPlugin, workspace.Completion{Lease: delivery.Lease, Status: "handled"}, 204, nil)

	claim := func() workspace.RunClaim {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/"+prefix+"runs/claim", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+device.Token)
		request.Header.Set("Muster-Run-Control", "1")
		request.Header.Set("Muster-Git-Verification", "1")
		request.Header.Set("Muster-Git-Publish", "1")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("领取运行返回 HTTP %d", response.StatusCode)
		}
		var run workspace.RunClaim
		if err := json.NewDecoder(response.Body).Decode(&run); err != nil {
			t.Fatal(err)
		}
		return run
	}
	record := func(run workspace.RunClaim, sequence int, event workspace.RunEvent) {
		t.Helper()
		event.Generation, event.Lease = run.Generation, run.Lease
		do("PUT", fmt.Sprintf("%sruns/%s/events/%d", prefix, run.ID, sequence), device.Token, event, 204, nil)
	}
	original := claim()
	if original.AgentKind != agentKind || !original.GitPublish || original.GitPolicy == nil {
		t.Fatalf("源任务缺少冻结的 Git 规则和推送授权: %+v", original)
	}
	record(original, 1, workspace.RunEvent{Kind: "started"})
	record(original, 2, workspace.RunEvent{Kind: "session", SessionID: sessionID})
	record(original, 3, workspace.RunEvent{
		Kind: "approval_requested", Approval: &approval.Question{Question: "保留 CSV 列名吗？"},
	})
	path := "tasks/" + original.TaskID
	var detail workspace.Detail
	do("GET", path, testAdmin, nil, 200, &detail)
	if len(detail.Approvals) != 1 || detail.TakeoverAllowed {
		t.Fatalf("等待答复的会话不应允许接手: %+v", detail)
	}
	do("POST", "approvals/"+detail.Approvals[0].ID+"/answer", testAdmin, map[string]string{"answer": "保留"}, 200, nil)
	resume := claim()
	if resume.SessionID != sessionID || !resume.SessionFailureSupported {
		t.Fatalf("续接轮缺少会话或分类能力: %+v", resume)
	}
	record(resume, 1, workspace.RunEvent{Kind: "started"})
	if agentKind == "codex" {
		var before, after workspace.Detail
		do("GET", path, testAdmin, nil, 200, &before)
		if before.Task.Status != "running" || len(before.Approvals) != 1 || before.Approvals[0].Status != "answered" ||
			before.Approvals[0].HandledAt != nil {
			t.Fatalf("当前续接运行应有一条未处理的请示答复: %+v", before)
		}
		do("PUT", fmt.Sprintf("%sruns/%s/events/2", prefix, resume.ID), device.Token, workspace.RunEvent{
			Kind: "failed", SessionFailure: "busy", Generation: resume.Generation, Lease: resume.Lease,
		}, 409, nil)
		do("GET", path, testAdmin, nil, 200, &after)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("拒绝 Codex 占用回执后任务或输入发生改变: %+v", after)
		}
	}
	failure := workspace.RunEvent{Kind: "failed", SessionFailure: "unavailable", Text: "原会话已丢失"}
	record(resume, 2, failure)
	record(resume, 2, failure)
	if agentKind == "codex" {
		do("PUT", fmt.Sprintf("%sruns/%s/events/3", prefix, resume.ID), device.Token, workspace.RunEvent{
			Kind: "failed", SessionFailure: "busy", Generation: resume.Generation, Lease: resume.Lease,
		}, 409, nil)
	}
	detail = workspace.Detail{}
	do("GET", path, testAdmin, nil, 200, &detail)
	if !detail.TakeoverAllowed || detail.Task.Status != "interrupted" || detail.Task.WaitingReason != "session_unavailable" ||
		len(detail.Approvals) != 1 || detail.Approvals[0].Status != "answered" || detail.Approvals[0].HandledAt != nil {
		t.Fatalf("会话不可用后应允许接手: %+v", detail)
	}

	takeover := workspace.TaskTakeover{
		RequestID: "20000000-0000-4000-8000-000000000003",
		Summary:   "已确认应保留 CSV 列名，继续核对修复和测试。",
	}
	do("PUT", path+"/takeover", device.Token, takeover, 401, nil)
	do("PUT", path+"/takeover", testPlugin, takeover, 401, nil)
	var created, replay workspace.Task
	do("PUT", path+"/takeover", testAdmin, takeover, 200, &created)
	do("PUT", path+"/takeover", testAdmin, takeover, 200, &replay)
	do("PUT", path+"/takeover", testAdmin, workspace.TaskTakeover{
		RequestID: takeover.RequestID, Summary: "不同摘要",
	}, 409, nil)
	if created.ID == original.TaskID || created.ID != replay.ID || created.RootTaskID != created.ID ||
		created.ParentTaskID != "" || created.TakeoverSourceTaskID != original.TaskID || created.GitPublish {
		t.Fatalf("新任务关系或授权错误: %+v", created)
	}

	next := claim()
	if next.TaskID != created.ID || next.SessionID != "" || next.Prompt != takeover.Summary ||
		next.AgentKind != original.AgentKind || next.ProjectID != original.ProjectID ||
		next.DirectoryID != original.DirectoryID || next.BindingID != original.BindingID ||
		next.GitPublish || !reflect.DeepEqual(next.GitPolicy, original.GitPolicy) {
		t.Fatalf("新运行应使用冻结绑定和规则启动独立会话: %+v", next)
	}
	detail = workspace.Detail{}
	do("GET", path, testAdmin, nil, 200, &detail)
	if detail.TakeoverAllowed || len(detail.Takeovers) != 1 || detail.Takeovers[0].ID != created.ID ||
		len(detail.Runs) != 2 || detail.Runs[1].Text != failure.Text || detail.Runs[1].SessionID != resume.SessionID {
		t.Fatalf("原任务应保留失败证据以及新任务链接: %+v", detail)
	}
}
