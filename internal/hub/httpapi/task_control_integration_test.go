package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

func TestTaskControlStopsAndResumesClaimedRun(t *testing.T) {
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
	directory := "dir-" + strings.Repeat("a", 32)
	do("PUT", prefix+"directories", device.Token,
		map[string]any{"directories": []workspace.Directory{{ID: directory, Label: "Folder"}}}, 204, nil)
	const project = "20000000-0000-4000-8000-000000000001"
	do("PUT", "projects/"+project, testAdmin, map[string]string{
		"name": "Project", "device_id": device.ID, "directory_id": directory,
	}, 200, nil)
	const message = "20000000-0000-4000-8000-000000000002"
	do("POST", "conversation/messages", testAdmin, map[string]string{"id": message, "text": "Run task"}, 202, nil)
	var delivery workspace.Delivery
	do("POST", "plugin/deliveries/claim", testPlugin, nil, 200, &delivery)
	do("POST", "plugin/deliveries/"+delivery.ID+"/dispatch", testPlugin, workspace.Dispatch{
		Lease: delivery.Lease, ProjectID: project, Prompt: "Run task", Title: "Task",
	}, 200, nil)
	controlPath := "tasks/task-" + message + "/control"
	const stopRequest = "20000000-0000-4000-8000-000000000003"
	stop := workspace.TaskControl{Action: "stop_current", RequestID: stopRequest}
	do("POST", controlPath, device.Token, stop, 401, nil)
	do("POST", controlPath, testPlugin, stop, 401, nil)
	do("POST", controlPath, testAdmin, workspace.TaskControl{Action: "explode", RequestID: stopRequest}, 400, nil)
	do("POST", controlPath, testAdmin, workspace.TaskControl{Action: "cancel", RequestID: "bad-request-id"}, 400, nil)

	claimRun := func() workspace.RunClaim {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/"+prefix+"runs/claim", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+device.Token)
		request.Header.Set("Muster-Run-Control", "1")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("claim HTTP %d", response.StatusCode)
		}
		var run workspace.RunClaim
		if err := json.NewDecoder(response.Body).Decode(&run); err != nil {
			t.Fatal(err)
		}
		if !run.ControlSupported {
			t.Fatal("Control capability missing from claimed run")
		}
		if !run.DiagnosticsSupported {
			t.Fatal("Diagnostics capability missing from claimed run")
		}
		return run
	}
	run := claimRun()
	runPath := prefix + "runs/" + run.ID
	const nativeSession = "native-task-control"
	do("PUT", runPath+"/events/1", device.Token, workspace.RunEvent{
		Kind: "started", SessionID: nativeSession, Generation: run.Generation, Lease: run.Lease,
	}, 204, nil)
	proof := map[string]any{"generation": run.Generation, "lease": run.Lease}
	do("POST", runPath+"/control", testAdmin, proof, 401, nil)
	do("POST", runPath+"/control", testPlugin, proof, 401, nil)
	do("POST", runPath+"/control", strings.Repeat("A", 43), proof, 401, nil)
	do("POST", runPath+"/control", device.Token, map[string]any{"generation": run.Generation + 1, "lease": run.Lease}, 409, nil)
	do("POST", runPath+"/control", device.Token, map[string]any{"generation": 0, "lease": run.Lease}, 400, nil)
	var directive struct{ Stop bool }
	do("POST", runPath+"/control", device.Token, proof, 200, &directive)
	if directive.Stop {
		t.Fatal("Stop instruction existed before admin request")
	}
	var before workspace.Snapshot
	do("GET", "workspace", testAdmin, nil, 200, &before)

	var task workspace.Task
	stopBody, err := json.Marshal(stop)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type controlResult struct {
		status int
		err    error
	}
	results := make(chan controlResult, 2)
	for range 2 {
		go func() {
			<-start
			var result controlResult
			defer func() { results <- result }()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/"+controlPath, bytes.NewReader(stopBody))
			if err != nil {
				result.err = err
				return
			}
			request.Header.Set("Authorization", "Bearer "+testAdmin)
			request.Header.Set("Content-Type", "application/json")
			response, err := server.Client().Do(request)
			result.err = err
			if err == nil {
				defer response.Body.Close()
				result.status = response.StatusCode
			}
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.status != http.StatusOK || result.err != nil {
			t.Fatalf("Concurrent stop request: status=%d error=%v", result.status, result.err)
		}
	}
	do("POST", controlPath, testAdmin, stop, 200, &task)
	if task.Status != "stopping" {
		t.Fatalf("Unexpected stopping task state %q", task.Status)
	}
	var after workspace.Snapshot
	do("GET", "workspace", testAdmin, nil, 200, &after)
	if after.Cursor != before.Cursor+1 {
		t.Fatalf("Repeated stop advanced the cursor more than once: %d to %d", before.Cursor, after.Cursor)
	}
	do("POST", controlPath, testAdmin, workspace.TaskControl{Action: "cancel", RequestID: stopRequest}, 409, nil)
	for range 2 {
		do("POST", runPath+"/control", device.Token, proof, 200, &directive)
		if !directive.Stop {
			t.Fatal("Stop instruction was lost before process exit")
		}
	}
	do("PUT", runPath+"/events/2", device.Token, workspace.RunEvent{Kind: "stopped"}, 400, nil)
	do("PUT", runPath+"/events/2", device.Token, map[string]any{
		"kind": "stopped", "generation": run.Generation, "lease": run.Lease,
		"diagnostics": map[string]any{"usage": map[string]any{"source": "dsh_headless", "input_tokens": 0}},
	}, 400, nil)
	exitCode := uint32(0)
	report := &runreport.Diagnostics{ExitCode: &exitCode, Usage: &runreport.Usage{Source: runreport.DSHHeadless}}
	for range 2 {
		do("PUT", runPath+"/events/2", device.Token, workspace.RunEvent{
			Kind: "stopped", SessionID: nativeSession, Generation: run.Generation, Lease: run.Lease, Diagnostics: report,
		}, 204, nil)
	}
	var detail workspace.Detail
	do("GET", "tasks/"+run.TaskID, testAdmin, nil, 200, &detail)
	if len(detail.Runs) != 1 || detail.Runs[0].Diagnostics == nil || detail.Runs[0].Diagnostics.ExitCode == nil ||
		*detail.Runs[0].Diagnostics.ExitCode != exitCode || detail.Runs[0].Diagnostics.Usage == nil {
		t.Fatalf("Missing stopped run diagnostics: %+v", detail.Runs)
	}
	do("POST", controlPath, testAdmin, stop, 200, &task)
	if task.Status != "paused" {
		t.Fatalf("Unexpected post-stop task state %q", task.Status)
	}
	const resumeRequest = "20000000-0000-4000-8000-000000000004"
	do("POST", controlPath, testAdmin, workspace.TaskControl{Action: "resume", RequestID: resumeRequest}, 200, &task)
	resumed := claimRun()
	if resumed.ID == run.ID || resumed.SessionID != nativeSession {
		t.Fatalf("Resume failed to create a new round in the native session: %+v", resumed)
	}
}
