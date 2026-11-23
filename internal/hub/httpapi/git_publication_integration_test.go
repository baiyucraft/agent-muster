package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func TestGitPublicationRequiresVerifiedIntentAndRemoteObservation(t *testing.T) {
	ctx, conn, schema, server, do := openWorkspaceFixture(t)
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
	directory := "dir-" + strings.Repeat("b", 32)
	do(http.MethodPut, prefix+"directories", device.Token, map[string]any{
		"directories": []workspace.Directory{{ID: directory, Label: "Git 项目"}},
	}, 204, nil)
	const project = "00000000-0000-4000-8000-000000000001"
	policy := gitdelivery.Policy{BaseRef: "refs/heads/main", Remote: "local", ReviewAgent: "codex", MaxRepairs: 1,
		Test: gitdelivery.Command{Program: "go", Args: []string{"test", "./..."}, TimeoutSeconds: 30}}
	do(http.MethodPut, "projects/"+project, testAdmin, map[string]any{
		"name": "测试项目", "device_id": device.ID, "directory_id": directory, "git_policy": policy,
	}, 200, nil)
	do(http.MethodPost, "conversation/messages", testAdmin, map[string]string{
		"id": "00000000-0000-4000-8000-000000000002", "text": "修复并推送",
	}, 202, nil)
	var delivery workspace.Delivery
	do(http.MethodPost, "plugin/deliveries/claim", testPlugin, nil, 200, &delivery)
	deliveryPath := "plugin/deliveries/" + delivery.ID
	request := workspace.Dispatch{Lease: delivery.Lease, ProjectID: project, Title: "修复并推送", Prompt: "修复并推送", GitPublish: true}
	do(http.MethodPost, deliveryPath+"/dispatch", testPlugin, request, 400, nil)
	request.GitDelivery = true
	do(http.MethodPost, deliveryPath+"/dispatch", testPlugin, request, 200, nil)
	do(http.MethodPut, deliveryPath+"/complete", testPlugin, workspace.Completion{Lease: delivery.Lease, Status: "handled"}, 204, nil)

	claim, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/"+prefix+"runs/claim", nil)
	if err != nil {
		t.Fatal(err)
	}
	claim.Header.Set("Authorization", "Bearer "+device.Token)
	claim.Header.Set("Muster-Run-Control", "1")
	claim.Header.Set("Muster-Git-Verification", "1")
	response, err := server.Client().Do(claim)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("仅支持验证的设备领取了推送任务，HTTP %d", response.StatusCode)
	}
	claim.Header.Set("Muster-Git-Publish", "1")
	response, err = server.Client().Do(claim)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var run workspace.RunClaim
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&run) != nil || !run.GitPublish {
		t.Fatalf("推送任务领取失败，HTTP %d，运行 %+v", response.StatusCode, run)
	}
	path := prefix + "runs/" + run.ID
	record := func(seq int, kind string, evidence *gitdelivery.Verification, expected int) {
		t.Helper()
		do(http.MethodPut, fmt.Sprintf("%s/events/%d", path, seq), device.Token, workspace.RunEvent{
			Kind: kind, Text: "运行结束", SessionID: "native", Lease: run.Lease, Generation: run.Generation, GitVerification: evidence,
		}, expected, nil)
	}
	zero := uint32(0)
	base, candidate, advanced := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	input := gitdelivery.PushRequest{Generation: run.Generation, Lease: run.Lease, DestinationFingerprint: strings.Repeat("d", 64),
		Verification: gitdelivery.Verification{BaseOID: base, CandidateOID: candidate, TestExitCode: &zero,
			TestLogRef: "test-1/output.log", TestStatus: "passed", ReviewAgent: "codex", ReviewStatus: "passed", RepairAttempts: 1}}
	authorize := path + "/git-push"
	do(http.MethodPost, authorize, device.Token, input, 409, nil)
	record(1, "started", nil, 204)
	record(2, "completed", &input.Verification, 409)
	bad := input
	bad.Verification.ReviewAgent = "dsh"
	do(http.MethodPost, authorize, device.Token, bad, 400, nil)
	bad = input
	bad.Verification.RepairAttempts = 2
	do(http.MethodPost, authorize, device.Token, bad, 400, nil)
	do(http.MethodPost, authorize, testPlugin, input, 401, nil)
	var intent, replay gitdelivery.PushIntent
	do(http.MethodPost, authorize, device.Token, input, 200, &intent)
	do(http.MethodPost, authorize, device.Token, input, 200, &replay)
	if intent != replay || !intent.PushAllowed || intent.Status != "pending" || intent.CandidateOID != candidate ||
		intent.Revision != 1 || intent.Remote != policy.Remote || intent.TargetRef != "refs/heads/muster/"+run.TaskID {
		t.Fatalf("推送规则或重放结果不一致: %+v, %+v", intent, replay)
	}
	bad = input
	bad.DestinationFingerprint = strings.Repeat("e", 64)
	do(http.MethodPost, authorize, device.Token, bad, 409, nil)
	do(http.MethodPost, "conversation/messages", testAdmin, map[string]string{
		"id": "00000000-0000-4000-8000-000000000003", "text": "改用另一个修复方案", "task_id": run.TaskID,
	}, 202, nil)
	do(http.MethodPost, authorize, device.Token, input, 200, &replay)
	if replay.PushAllowed || replay.ID != intent.ID {
		t.Fatalf("已有用户追加要求时，只能读取推送意图: %+v", replay)
	}
	if _, err := conn.Exec(ctx, "UPDATE "+schema+".muster_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", run.ID); err != nil {
		t.Fatal(err)
	}
	var control struct{ Stop bool }
	do(http.MethodPost, path+"/control", device.Token, map[string]any{"generation": run.Generation, "lease": run.Lease}, 200, &control)
	if control.Stop {
		t.Fatal("没有停止指令时，恢复回读不应停止")
	}
	observed := gitdelivery.Observation{Generation: run.Generation, Lease: run.Lease, Status: "same_commit", ObservedOID: advanced}
	observe := authorize + "/" + intent.ID + "/observation"
	do(http.MethodPut, observe, device.Token, observed, 400, nil)
	observed.Status, observed.ObservedOID = "unavailable", ""
	do(http.MethodPut, observe, device.Token, observed, 200, &replay)
	if replay.Status != "unavailable" || replay.PushAllowed {
		t.Fatalf("不可读取的远端应保留待确认状态: %+v", replay)
	}
	record(2, "completed", &input.Verification, 409)
	observed.Status, observed.ObservedOID = "contains_commit", advanced
	do(http.MethodPut, observe, device.Token, observed, 200, &replay)
	if replay.Status != "contains_commit" || replay.ObservedOID != advanced || replay.PushAllowed {
		t.Fatalf("过期租约的回读应只保存远端证据: %+v", replay)
	}
	do(http.MethodPut, observe, device.Token, gitdelivery.Observation{
		Generation: run.Generation, Lease: run.Lease, Status: "missing",
	}, 200, &replay)
	if replay.Status != "contains_commit" || replay.ObservedOID != advanced {
		t.Fatalf("已经确认的回读被后续结果覆盖: %+v", replay)
	}
	bad = input
	bad.Verification.CandidateOID = advanced
	record(2, "completed", &bad.Verification, 409)
	record(2, "completed", &input.Verification, 204)
	record(2, "completed", &input.Verification, 204)
	var detail map[string]any
	do(http.MethodGet, "tasks/"+run.TaskID, testAdmin, nil, 200, &detail)
	publication := detail["runs"].([]any)[0].(map[string]any)["git_publication"].(map[string]any)
	if publication["status"] != "contains_commit" || publication["observed_oid"] != advanced ||
		publication["destination_fingerprint"] != nil || publication["push_allowed"] != nil {
		t.Fatalf("管理员任务详情应仅展示公开回读信息: %+v", publication)
	}
	var report string
	if err := conn.QueryRow(ctx, "SELECT body FROM "+schema+".muster_deliveries WHERE id=$1", "report-"+run.ID).Scan(&report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Git 测试通过", "独立 Review 通过（codex）", candidate, advanced, intent.TargetRef} {
		if !strings.Contains(report, expected) {
			t.Fatalf("根任务汇报缺少已保存的推送证据 %q", expected)
		}
	}

	// 旧版设备仍按原协议交付追加要求，Hub 保留实际无法确认处理的状态。
	do(http.MethodPost, "plugin/deliveries/claim", testPlugin, nil, 200, &delivery)
	if delivery.Kind == "report" {
		do(http.MethodPut, "plugin/deliveries/"+delivery.ID+"/complete", testPlugin,
			workspace.Completion{Lease: delivery.Lease, Status: "handled", Text: "已确认远端"}, 204, nil)
		do(http.MethodPost, "plugin/deliveries/claim", testPlugin, nil, 200, &delivery)
	}
	if delivery.Kind != "message" || delivery.TaskID != run.TaskID {
		t.Fatalf("未领取原任务的追加要求: %+v", delivery)
	}
	do(http.MethodPost, "plugin/deliveries/"+delivery.ID+"/dispatch", testPlugin,
		workspace.Dispatch{Lease: delivery.Lease, TaskID: run.TaskID, Prompt: "使用另一个修复方案"}, 200, nil)
	do(http.MethodPut, "plugin/deliveries/"+delivery.ID+"/complete", testPlugin,
		workspace.Completion{Lease: delivery.Lease, Status: "handled"}, 204, nil)
	response, err = server.Client().Do(claim)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var followUp workspace.RunClaim
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&followUp) != nil ||
		followUp.Prompt != "使用另一个修复方案" || len(followUp.InputIDs) != 0 {
		t.Fatalf("旧版设备无法领取追加要求，HTTP %d，运行 %+v", response.StatusCode, followUp)
	}
	run = followUp
	path = prefix + "runs/" + run.ID
	record(1, "started", nil, 204)
	input.Generation, input.Lease = run.Generation, run.Lease
	do(http.MethodPost, path+"/git-push", device.Token, input, 200, &intent)
	observed.Generation, observed.Lease = run.Generation, run.Lease
	do(http.MethodPut, path+"/git-push/"+intent.ID+"/observation", device.Token, observed, 200, nil)
	record(2, "completed", &input.Verification, 204)
	var receipts workspace.Detail
	do(http.MethodGet, "tasks/"+run.TaskID, testAdmin, nil, 200, &receipts)
	if len(receipts.Inputs) != 2 || receipts.Inputs[0].ReceiptStatus != "" || receipts.Inputs[1].ReceiptStatus != "needs_review" ||
		receipts.Inputs[1].ReceiptRunID != followUp.ID || receipts.Inputs[1].HandledAt != nil {
		t.Fatalf("旧协议完成后应保留待核对输入: %+v", receipts.Inputs)
	}
}
