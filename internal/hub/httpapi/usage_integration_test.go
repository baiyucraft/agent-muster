package httpapi

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

func TestDeviceUsageCoverage(t *testing.T) {
	ctx, conn, schema, _, do := openWorkspaceFixture(t)
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema); err != nil {
		t.Fatal(err)
	}
	var empty workspace.DeviceUsage
	do(http.MethodGet, "workspace/usage", testAdmin, nil, http.StatusOK, &empty)
	if empty.Projects == nil || empty.Devices == nil || len(empty.Projects)+len(empty.Devices) != 0 {
		t.Fatalf("空用量汇总应返回两个空列表：%+v", empty)
	}
	enroll := func(name string) (id, token string) {
		t.Helper()
		var pairing struct{ Code string }
		do(http.MethodPost, "pairing-codes", testAdmin, map[string]string{"name": name}, http.StatusCreated, &pairing)
		var device struct {
			ID    string `json:"device_id"`
			Token string
		}
		do(http.MethodPost, "devices/enroll", "", map[string]string{
			"code": pairing.Code, "hostname": "test", "os": "windows", "arch": "amd64",
		}, http.StatusCreated, &device)
		return device.ID, device.Token
	}
	first, token := enroll("设备 A")
	second, _ := enroll("设备 B")
	do(http.MethodGet, "workspace/usage", "", nil, http.StatusUnauthorized, nil)
	do(http.MethodGet, "workspace/usage", testPlugin, nil, http.StatusUnauthorized, nil)
	do(http.MethodGet, "workspace/usage", token, nil, http.StatusUnauthorized, nil)
	const projectA = "00000000-0000-4000-8000-000000000001"
	const projectB = "00000000-0000-4000-8000-000000000002"
	dir := "dir-" + strings.Repeat("b", 32)
	do(http.MethodPut, "devices/"+first+"/directories", token,
		map[string]any{"directories": []workspace.Directory{{ID: dir, Label: "用量项目"}}}, http.StatusNoContent, nil)
	do(http.MethodPut, "projects/"+projectA, testAdmin,
		map[string]string{"name": "项目 A", "device_id": first, "directory_id": dir}, http.StatusOK, nil)
	do(http.MethodPut, "projects/"+projectB, testAdmin,
		map[string]string{"name": "项目 B", "device_id": first, "directory_id": dir}, http.StatusOK, nil)
	const taskA = "task-00000000-0000-4000-8000-000000000003"
	const taskB = "task-00000000-0000-4000-8000-000000000004"
	for task, project := range map[string]string{taskA: projectA, taskB: projectB} {
		if _, err := conn.Exec(ctx, `INSERT INTO muster_tasks
			(id, root_id, project_id, project_name, device_id, directory_id, title, status, current_run_id, binding_id)
			SELECT $1,$1,$2,'旧项目名',$3,$4,'用量任务','completed','',id
			FROM muster_project_bindings WHERE project_id=$2 AND is_default`, task, project, first, dir); err != nil {
			t.Fatal(err)
		}
	}
	var zero int64
	codex := &runreport.Diagnostics{Usage: &runreport.Usage{Source: runreport.CodexExec, InputTokens: 1<<53 - 1, CachedInputTokens: &zero}}
	dsh := &runreport.Diagnostics{Usage: &runreport.Usage{Source: runreport.DSHHeadless, InputTokens: 0, OutputTokens: 4, TotalTokens: &zero}}
	for _, fixture := range []struct {
		id, task, device, status string
		claimed                  bool
		diagnostics              *runreport.Diagnostics
	}{
		{"finished-codex-one", taskA, first, "completed", true, codex},
		{"finished-codex-two", taskA, first, "stopped", true, codex},
		{"finished-dsh", taskA, second, "failed", true, dsh},
		{"finished-legacy", taskA, second, "waiting", true, nil},
		{"interrupted", taskB, first, "interrupted", true, nil},
		{"active", taskA, first, "running", true, nil},
		{"queued", taskA, first, "queued", true, nil},
		{"never-claimed", taskA, first, "cancelled", false, nil},
	} {
		if _, err := conn.Exec(ctx, `INSERT INTO muster_runs (id,task_id,device_id,input,status,claimed_at,diagnostics)
			VALUES ($1,$2,$3,'用量任务',$4,CASE WHEN $5 THEN clock_timestamp() END,$6)`,
			fixture.id, fixture.task, fixture.device, fixture.status, fixture.claimed, fixture.diagnostics); err != nil {
			t.Fatal(err)
		}
	}
	// 运行事件仅用于审计，迟到的回执不能重复计入运行行的用量。
	if _, err := conn.Exec(ctx, `INSERT INTO muster_run_events (run_id,sequence,payload) VALUES
		('finished-codex-one',1,$1),('finished-codex-one',2,$1)`, workspace.RunEvent{Kind: "completed", Diagnostics: codex}); err != nil {
		t.Fatal(err)
	}
	var usage workspace.DeviceUsage
	do(http.MethodGet, "workspace/usage", testAdmin, nil, http.StatusOK, &usage)
	zeroSum := "0"
	codexUsage := workspace.UsageSource{
		Source: runreport.CodexExec, MeasuredRuns: 2, InputTokens: "18014398509481982", OutputTokens: "0",
		CachedInputTokens: &zeroSum, CachedInputRuns: 2,
	}
	dshUsage := workspace.UsageSource{
		Source: runreport.DSHHeadless, MeasuredRuns: 1, InputTokens: "0", OutputTokens: "4", TotalTokens: &zeroSum, TotalRuns: 1,
	}
	want := workspace.DeviceUsage{
		Projects: []workspace.UsageGroup{
			{ID: projectA, Name: "项目 A", FinishedRuns: 4, MeasuredRuns: 3, Sources: []workspace.UsageSource{codexUsage, dshUsage}},
			{ID: projectB, Name: "项目 B", FinishedRuns: 1, Sources: []workspace.UsageSource{}},
		},
		Devices: []workspace.UsageGroup{
			{ID: first, Name: "设备 A", FinishedRuns: 3, MeasuredRuns: 2, Sources: []workspace.UsageSource{codexUsage}},
			{ID: second, Name: "设备 B", FinishedRuns: 2, MeasuredRuns: 1, Sources: []workspace.UsageSource{dshUsage}},
		},
	}
	if !reflect.DeepEqual(usage, want) {
		t.Fatalf("设备用量及覆盖数不符：\n实际 %+v\n期望 %+v", usage, want)
	}
}
