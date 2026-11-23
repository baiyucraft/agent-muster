package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestManagedGitDispatchFreezesPolicyAndRequiresVerification(t *testing.T) {
	ctx, _, _, server, do := openWorkspaceFixture(t)
	var pairing struct{ Code string }
	do(http.MethodPost, "pairing-codes", testAdmin, map[string]string{"name": "开发设备"}, 201, &pairing)
	var device struct {
		ID    string `json:"device_id"`
		Token string
	}
	do(http.MethodPost, "devices/enroll", "", map[string]string{
		"code": pairing.Code, "hostname": "test", "os": "windows", "arch": "amd64",
	}, 201, &device)
	prefix := "devices/" + device.ID + "/"
	dir := "dir-" + strings.Repeat("a", 32)
	do(http.MethodPut, prefix+"directories", device.Token, map[string]any{
		"directories": []workspace.Directory{{ID: dir, Label: "Git 项目"}},
	}, 204, nil)
	const projectID = "00000000-0000-4000-8000-000000000001"
	projectPath := "projects/" + projectID
	project := map[string]any{"name": "测试项目", "device_id": device.ID, "directory_id": dir}
	do(http.MethodPut, projectPath, testAdmin, project, 200, nil)
	dispatch := func(id string, managed bool, expected int) string {
		t.Helper()
		do(http.MethodPost, "conversation/messages", testAdmin, map[string]string{"id": id, "text": "修复项目"}, 202, nil)
		var delivery workspace.Delivery
		do(http.MethodPost, "plugin/deliveries/claim", testPlugin, nil, 200, &delivery)
		var result struct {
			TaskID string `json:"task_id"`
		}
		path := "plugin/deliveries/" + delivery.ID
		input := workspace.Dispatch{Lease: delivery.Lease, ProjectID: projectID, Title: "修复项目", Prompt: "修复项目", GitDelivery: managed}
		if expected == 200 {
			do(http.MethodPost, path+"/dispatch", testPlugin, input, expected, &result)
		} else {
			do(http.MethodPost, path+"/dispatch", testPlugin, input, expected, nil)
		}
		do(http.MethodPut, path+"/complete", testPlugin, workspace.Completion{Lease: delivery.Lease, Status: "handled"}, 204, nil)
		return result.TaskID
	}
	dispatch("00000000-0000-4000-8000-000000000002", true, 409)
	policy := gitdelivery.Policy{BaseRef: "refs/heads/main", Remote: "origin", MaxRepairs: 1,
		CandidateCommit: gitdelivery.CandidateCommitDaemon,
		Test:            gitdelivery.Command{Program: "go", Args: []string{"test", "./..."}, TimeoutSeconds: 30}}
	project["git_policy"] = policy
	do(http.MethodPut, projectPath, testAdmin, project, 200, nil)
	managed := dispatch("00000000-0000-4000-8000-000000000003", true, 200)
	ordinary := dispatch("00000000-0000-4000-8000-000000000004", false, 200)
	project["git_policy"] = nil
	do(http.MethodPut, projectPath, testAdmin, project, 200, nil)
	var oldRun workspace.RunClaim
	do(http.MethodPost, prefix+"runs/claim", device.Token, nil, 200, &oldRun)
	if oldRun.TaskID != ordinary || oldRun.GitPolicy != nil {
		t.Fatalf("旧设备应只得到普通任务: %+v", oldRun)
	}
	record := func(run workspace.RunClaim, sequence int, kind string, evidence *gitdelivery.Verification, expected int) {
		t.Helper()
		event := workspace.RunEvent{Kind: kind, Text: "测试结束", Lease: run.Lease, Generation: run.Generation, GitVerification: evidence}
		do(http.MethodPut, fmt.Sprintf("%sruns/%s/events/%d", prefix, run.ID, sequence), device.Token, event, expected, nil)
	}
	zero := uint32(0)
	evidence := &gitdelivery.Verification{BaseOID: strings.Repeat("a", 40), CandidateOID: strings.Repeat("b", 40),
		TestExitCode: &zero, TestLogRef: "test-123/output.log", TestStatus: "passed"}
	record(oldRun, 1, "completed", evidence, 400)
	record(oldRun, 1, "completed", nil, 204)
	claim := func(candidateCommit bool, want int) (run workspace.RunClaim) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/"+prefix+"runs/claim", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+device.Token)
		request.Header.Set("Muster-Run-Control", "1")
		request.Header.Set("Muster-Git-Verification", "1")
		if candidateCommit {
			request.Header.Set(gitdelivery.CandidateCommitHeader, "1")
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("托管任务领取 HTTP %d，期望 %d", response.StatusCode, want)
		}
		if want == http.StatusOK {
			if err := json.NewDecoder(response.Body).Decode(&run); err != nil {
				t.Fatal(err)
			}
		}
		return run
	}
	claim(false, http.StatusNoContent)
	run := claim(true, http.StatusOK)
	policy.Revision = 1
	if run.TaskID != managed || run.GitPolicy == nil || !reflect.DeepEqual(*run.GitPolicy, policy) {
		t.Fatalf("托管任务未保持派发时的项目规则: %+v", run)
	}
	record(run, 1, "started", evidence, 400)
	record(run, 1, "started", nil, 204)
	record(run, 2, "completed", nil, 400)
	record(run, 2, "completed", &gitdelivery.Verification{TestStatus: "unverified"}, 400)
	record(run, 2, "completed", evidence, 204)
	record(run, 2, "completed", evidence, 204)
	changed := *evidence
	changed.CandidateOID = strings.Repeat("c", 40)
	record(run, 2, "completed", &changed, 409)
	var detail workspace.Detail
	do(http.MethodGet, "tasks/"+managed, testAdmin, nil, 200, &detail)
	if detail.Task.Status != "completed" || detail.Task.GitPolicyRevision != 1 || len(detail.Runs) != 1 ||
		!reflect.DeepEqual(detail.Runs[0].GitVerification, evidence) {
		t.Fatalf("任务详情没有返回冻结版本和已保存验证: %+v", detail)
	}
}
