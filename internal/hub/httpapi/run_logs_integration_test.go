package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

func TestRunLogDeviceAuthorizationAndDelivery(t *testing.T) {
	ctx, conn, schema, server, do := openWorkspaceFixture(t)
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema); err != nil {
		t.Fatal(err)
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
	device, token := enroll("执行设备")
	other, otherToken := enroll("另一个设备")
	ownerPath := "devices/" + device + "/run-log-requests"
	otherPath := "devices/" + other + "/run-log-requests"
	const project = "00000000-0000-4000-8000-000000000001"
	const taskID = "task-00000000-0000-4000-8000-000000000002"
	const runID = "run-00000000-0000-4000-8000-000000000003"
	directory := "dir-" + strings.Repeat("a", 32)
	do(http.MethodPut, "devices/"+device+"/directories", token,
		map[string]any{"directories": []workspace.Directory{{ID: directory, Label: "日志项目"}}}, http.StatusNoContent, nil)
	do(http.MethodPut, "projects/"+project, testAdmin,
		map[string]string{"name": "日志项目", "device_id": device, "directory_id": directory}, http.StatusOK, nil)
	if _, err := conn.Exec(ctx, `
		INSERT INTO muster_tasks (id,root_id,project_id,project_name,device_id,directory_id,title,status,current_run_id,binding_id)
		SELECT $1,$1,$2,'日志项目',$3,$4,'读取日志','completed',$5,id
		FROM muster_project_bindings WHERE project_id=$2 AND is_default`, taskID, project, device, directory, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO muster_runs (id,task_id,device_id,input,status,claimed_at)
		VALUES ($1,$2,$3,'读取日志','running',clock_timestamp())`, runID, taskID, device); err != nil {
		t.Fatal(err)
	}
	adminPath := "tasks/" + taskID + "/runs/" + runID + "/logs"
	do(http.MethodGet, adminPath, token, nil, http.StatusUnauthorized, nil)
	do(http.MethodGet, adminPath, testPlugin, nil, http.StatusUnauthorized, nil)
	do(http.MethodGet, ownerPath, testAdmin, nil, http.StatusUnauthorized, nil)
	do(http.MethodGet, ownerPath, otherToken, nil, http.StatusUnauthorized, nil)
	do(http.MethodGet, adminPath, testAdmin, nil, http.StatusConflict, nil)
	do(http.MethodGet, "tasks/task-"+project+"/runs/"+runID+"/logs", testAdmin, nil, http.StatusNotFound, nil)
	do(http.MethodHead, adminPath, testAdmin, nil, http.StatusMethodNotAllowed, nil)
	if _, err := conn.Exec(ctx, "UPDATE muster_runs SET status='completed' WHERE id=$1", runID); err != nil {
		t.Fatal(err)
	}
	do(http.MethodGet, adminPath, testAdmin, nil, http.StatusGatewayTimeout, nil)
	do(http.MethodPost, "devices/"+device+"/heartbeat", token, nil, http.StatusNoContent, nil)

	poll := func(path, credential string) []runreport.LogRequest {
		t.Helper()
		var result struct{ Requests []runreport.LogRequest }
		do(http.MethodGet, path, credential, nil, http.StatusOK, &result)
		if result.Requests == nil {
			t.Fatal("日志轮询未返回请求数组")
		}
		return result.Requests
	}
	waitFor := func(size int) []runreport.LogRequest {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if requests := poll(ownerPath, token); len(requests) == size {
				return requests
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("执行设备没有取得 %d 条请求", size)
		return nil
	}
	type readResult struct {
		response *http.Response
		err      error
	}
	startRead := func() (context.CancelFunc, <-chan readResult) {
		t.Helper()
		requestCtx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, server.URL+"/api/v1/"+adminPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+testAdmin)
		completion := make(chan readResult, 1)
		go func() {
			response, err := server.Client().Do(request)
			completion <- readResult{response: response, err: err}
		}()
		return cancel, completion
	}
	_, completion := startRead()
	queued := waitFor(1)[0]
	if queued.RunID != runID || queued.Validate() != nil || len(poll(otherPath, otherToken)) != 0 {
		t.Fatalf("日志请求未绑定当前运行和执行设备 (%+v)", queued)
	}
	want := runreport.LogResponse{Status: runreport.LogAvailable, Files: []runreport.LogFile{{
		Invocation: "cli-1", Stream: "stdout", Text: "本机 CLI 输出\n", Truncated: true,
	}}}
	do(http.MethodPut, otherPath+"/"+queued.ID, otherToken, want, http.StatusNotFound, nil)
	do(http.MethodPut, ownerPath+"/"+queued.ID, token,
		runreport.LogResponse{Status: runreport.LogAvailable}, http.StatusBadRequest, nil)
	tooLarge := want
	tooLarge.Files = []runreport.LogFile{{Invocation: "cli-1", Stream: "stdout", Text: strings.Repeat("a", runreport.MaxLogTextBytes+1)}}
	do(http.MethodPut, ownerPath+"/"+queued.ID, token, tooLarge, http.StatusBadRequest, nil)
	tooLarge.Files[0].Text = strings.Repeat("b", runreport.MaxLogResponseBytes)
	do(http.MethodPut, ownerPath+"/"+queued.ID, token, tooLarge, http.StatusRequestEntityTooLarge, nil)
	do(http.MethodPut, ownerPath+"/"+queued.ID, token, want, http.StatusNoContent, nil)
	result := <-completion
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.response.Body.Close()
	if result.response.StatusCode != http.StatusOK || result.response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("管理员日志响应不正确 (%d, %v)", result.response.StatusCode, result.response.Header)
	}
	var response runreport.LogResponse
	if err := json.NewDecoder(result.response.Body).Decode(&response); err != nil || !reflect.DeepEqual(response, want) {
		t.Fatalf("日志转发不正确 (%+v, %v)", response, err)
	}
	do(http.MethodPut, ownerPath+"/"+queued.ID, token, want, http.StatusNotFound, nil)

	cancel, completion := startRead()
	queued = waitFor(1)[0]
	cancel()
	cancelled := <-completion
	if cancelled.response != nil {
		cancelled.response.Body.Close()
	}
	waitFor(0)
	do(http.MethodPut, ownerPath+"/"+queued.ID, token, want, http.StatusNotFound, nil)

	cancel, completion = startRead()
	queued = waitFor(1)[0]
	do(http.MethodDelete, "devices/"+device, testAdmin, nil, http.StatusNoContent, nil)
	do(http.MethodGet, ownerPath, token, nil, http.StatusUnauthorized, nil)
	do(http.MethodPut, ownerPath+"/"+queued.ID, token, want, http.StatusUnauthorized, nil)
	do(http.MethodGet, adminPath, testAdmin, nil, http.StatusConflict, nil)
	cancel()
	cancelled = <-completion
	if cancelled.response != nil {
		cancelled.response.Body.Close()
	}
}
