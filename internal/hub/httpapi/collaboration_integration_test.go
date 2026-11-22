package httpapi

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/peer"
)

func TestCollaborationEndToEnd(t *testing.T) {
	_, _, _, _, do := openWorkspaceFixture(t)
	type identity struct {
		ID    string `json:"device_id"`
		Token string `json:"token"`
	}
	enroll := func(name string, port int) identity {
		t.Helper()
		var pairing struct{ Code string }
		do("POST", "pairing-codes", testAdmin, map[string]string{"name": name}, 201, &pairing)
		var device identity
		do("POST", "devices/enroll", "", map[string]string{
			"code": pairing.Code, "hostname": name, "os": "windows", "arch": "amd64",
		}, 201, &device)
		do("PUT", "devices/"+device.ID+"/peer", device.Token, map[string]string{
			"address": fmt.Sprintf("https://127.0.0.1:%d", port), "fingerprint": strings.Repeat("a", 64),
		}, 204, nil)
		return device
	}
	a, b := enroll("home", 19001), enroll("office", 19002)
	dir := "dir-" + strings.Repeat("a", 32)
	for _, device := range []identity{a, b} {
		do("PUT", "devices/"+device.ID+"/directories", device.Token, map[string]any{
			"directories": []workspace.Directory{{ID: dir, Label: "Fixture"}},
		}, 204, nil)
	}
	const project = "00000000-0000-4000-8000-000000000010"
	const home = "00000000-0000-4000-8000-000000000011"
	const office = "00000000-0000-4000-8000-000000000012"
	var saved workspace.Project
	do("PUT", "projects/"+project, testAdmin, workspace.Project{
		Name: "Collaboration",
		Bindings: []workspace.Binding{
			{ID: home, DeviceID: a.ID, DirectoryID: dir, AgentKind: "dsh", IsDefault: true},
			{ID: office, DeviceID: b.ID, DirectoryID: dir, AgentKind: "codex"},
		},
	}, 200, &saved)
	if saved.DeviceID != a.ID || len(saved.Bindings) != 2 {
		t.Fatalf("unexpected project bindings: %+v", saved)
	}
	claimDelivery := func() workspace.Delivery {
		t.Helper()
		var value workspace.Delivery
		do("POST", "plugin/deliveries/claim", testPlugin, nil, 200, &value)
		return value
	}
	createRoot := func(index int, binding string) string {
		t.Helper()
		do("POST", "conversation/messages", testAdmin, map[string]string{
			"id":   fmt.Sprintf("00000000-0000-4000-8000-%012d", index),
			"text": "Prepare the test index",
		}, 202, nil)
		delivery := claimDelivery()
		var result struct {
			TaskID string `json:"task_id"`
		}
		do("POST", "plugin/deliveries/"+delivery.ID+"/dispatch", testPlugin, workspace.Dispatch{
			Lease: delivery.Lease, ProjectID: project, BindingID: binding, Title: "Index", Prompt: "Prepare index",
		}, 200, &result)
		do("PUT", "plugin/deliveries/"+delivery.ID+"/complete", testPlugin, workspace.Completion{
			Lease: delivery.Lease, Status: "handled",
		}, 204, nil)
		return result.TaskID
	}
	claim := func(device identity) workspace.RunClaim {
		t.Helper()
		var run workspace.RunClaim
		do("POST", "devices/"+device.ID+"/runs/claim", device.Token, nil, 200, &run)
		return run
	}
	runPath := func(device identity, run workspace.RunClaim) string {
		return "devices/" + device.ID + "/runs/" + run.ID
	}
	event := func(device identity, run workspace.RunClaim, seq int, value workspace.RunEvent, code int) {
		t.Helper()
		value.Lease, value.Generation = run.Lease, run.Generation
		do("PUT", fmt.Sprintf("%s/events/%d", runPath(device, run), seq), device.Token, value, code, nil)
	}
	makeTransfer := func(from, to identity, run workspace.RunClaim, index int, kind string) workspace.Transfer {
		t.Helper()
		var manifest workspace.Transfer
		do("POST", runPath(from, run)+"/transfers", from.Token, workspace.TransferRequest{
			ID: fmt.Sprintf("transfer-00000000-0000-4000-8000-%012d", index), TargetDeviceID: to.ID,
			Lease: run.Lease, Generation: run.Generation, Kind: kind, Name: kind + ".txt",
			Size: 7, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("fixture"))),
		}, 201, &manifest)
		return manifest
	}
	authorize := func(device identity, manifest workspace.Transfer) workspace.Transfer {
		t.Helper()
		var auth workspace.Transfer
		do("POST", "devices/"+device.ID+"/transfers/"+manifest.ID+"/authorize", device.Token, nil, 200, &auth)
		return auth
	}
	ack := func(receiver identity, manifest workspace.Transfer) {
		t.Helper()
		path := "devices/" + receiver.ID + "/transfers/" + manifest.ID
		body := map[string]string{"ticket": manifest.Ticket}
		do("POST", path+"/verify", receiver.Token, body, 200, nil)
		do("PUT", path+"/ack", receiver.Token, body, 204, nil)
		do("PUT", path+"/ack", receiver.Token, body, 204, nil)
	}

	parentID := createRoot(20, home)
	parent := claim(a)
	if parent.TaskID != parentID || parent.AgentKind != "dsh" {
		t.Fatalf("unexpected parent route: %+v", parent)
	}
	do("PUT", runPath(a, parent)+"/lease", a.Token, map[string]any{
		"lease": parent.Lease, "generation": parent.Generation + 1,
	}, 409, nil)
	do("PUT", fmt.Sprintf("%s/events/1", runPath(a, parent)), a.Token, workspace.RunEvent{
		Kind: "started", Generation: parent.Generation, Lease: strings.Repeat("B", 26),
	}, 409, nil)
	event(a, parent, 1, workspace.RunEvent{Kind: "started"}, 204)
	event(a, parent, 2, workspace.RunEvent{Kind: "session", SessionID: "parent-native-session"}, 204)
	do("PUT", runPath(a, parent)+"/lease", a.Token, map[string]any{
		"lease": parent.Lease, "generation": parent.Generation,
	}, 200, nil)

	parallelID := createRoot(21, office)
	parallel := claim(b)
	if parallel.TaskID != parallelID || parallel.AgentKind != "codex" {
		t.Fatalf("unexpected second root route: %+v", parallel)
	}
	event(b, parallel, 1, workspace.RunEvent{Kind: "started"}, 204)

	prompt := makeTransfer(a, b, parent, 30, "prompt")
	file := makeTransfer(a, b, parent, 31, "file")
	c := enroll("unrelated", 19003)
	key := map[string]string{"public_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE="}
	otherKey := map[string]string{"public_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAI="}
	keyPath := "/rtc/public-key"
	t.Setenv("MUSTER_RTC_KEY_TRANSFER_ID", "")
	do("PUT", "devices/"+a.ID+keyPath, a.Token, key, 404, nil)
	t.Setenv("MUSTER_RTC_KEY_TRANSFER_ID", file.ID)
	do("PUT", "devices/"+a.ID+keyPath, a.Token, map[string]string{"public_key": "not-an-ed25519-key"}, 400, nil)
	do("PUT", "devices/"+a.ID+keyPath, testAdmin, key, 401, nil)
	do("PUT", "devices/"+a.ID+keyPath, testPlugin, key, 401, nil)
	do("PUT", "devices/"+a.ID+keyPath, b.Token, key, 401, nil)
	do("PUT", "devices/"+c.ID+keyPath, c.Token, key, 404, nil)
	do("PUT", "devices/"+a.ID+keyPath, a.Token, key, 204, nil)
	do("PUT", "devices/"+a.ID+keyPath, a.Token, key, 204, nil)
	do("PUT", "devices/"+a.ID+keyPath, a.Token, otherKey, 409, nil)
	do("PUT", "devices/"+b.ID+keyPath, b.Token, otherKey, 204, nil)
	t.Setenv("MUSTER_RTC_STUN_URLS", "")
	t.Setenv("MUSTER_RTC_TURN_URLS", "")
	t.Setenv("MUSTER_RTC_TURN_REST_SECRET", "")
	t.Setenv("MUSTER_RTC_TURN_DEVICE_IDS", "")
	bRTC := "devices/" + b.ID + "/rtc"
	aRTC := "devices/" + a.ID + "/rtc"
	cRTC := "devices/" + c.ID + "/rtc"
	offer := aRTC + "/" + file.ID + "/offer"
	answer := "/" + file.ID + "/answer"
	signal := peer.RTCSignal{
		AttemptID: strings.Repeat("a", 32),
		SDP:       "v=0\r\na=fingerprint:sha-256 AA:BB\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n",
	}
	do("PUT", offer, a.Token, signal, 409, nil)
	do("PUT", bRTC, testPlugin, nil, 401, nil)
	do("PUT", bRTC, c.Token, nil, 401, nil)
	// ICE 配置无效时，登记失败的设备不能成为新的交接目标。
	t.Setenv("MUSTER_RTC_STUN_URLS", "invalid")
	do("PUT", bRTC, b.Token, nil, 503, nil)
	t.Setenv("MUSTER_RTC_STUN_URLS", "")
	do("PUT", offer, a.Token, signal, 409, nil)
	var config peer.RTCConfig
	do("PUT", bRTC, b.Token, nil, 200, &config)
	if config.ICEServers == nil || len(config.ICEServers) != 0 {
		t.Fatalf("expected configuration to disable public STUN and TURN: %+v", config)
	}
	do("PUT", cRTC+"/"+file.ID+"/offer", c.Token, signal, 409, nil)
	do("PUT", offer, a.Token, signal, 204, nil)
	var offers peer.RTCOffers
	do("GET", cRTC+"/offers", c.Token, nil, 200, &offers)
	if len(offers.Offers) != 0 {
		t.Fatalf("unrelated device received signaling metadata: %+v", offers)
	}
	do("GET", bRTC+"/offers", b.Token, nil, 200, &offers)
	if len(offers.Offers) != 1 || offers.Offers[0] != (peer.RTCOffer{TransferID: file.ID, RTCSignal: signal}) {
		t.Fatalf("receiver did not get the matching offer: %+v", offers)
	}
	do("PUT", cRTC+answer, c.Token, signal, 409, nil)
	do("PUT", bRTC+answer, b.Token, signal, 204, nil)
	do("PUT", bRTC+answer, b.Token, signal, 204, nil)
	diagnosticPath := "/" + file.ID + "/diagnostic"
	diagnostic := peer.RTCDiagnostic{AttemptID: signal.AttemptID, Stage: "wait_stream", ICEState: "failed", RequestsSent: 8}
	t.Setenv("MUSTER_RTC_DIAGNOSTIC_TRANSFER_ID", "")
	t.Setenv("MUSTER_RTC_DIAGNOSTIC_PORT_MAPPING", "1")
	do("PUT", aRTC+diagnosticPath, a.Token, diagnostic, 404, nil)
	t.Setenv("MUSTER_RTC_DIAGNOSTIC_TRANSFER_ID", file.ID)
	for _, device := range []identity{a, b, c} {
		do("PUT", "devices/"+device.ID+"/rtc", device.Token, nil, 200, &config)
		want := file.ID
		if device == c {
			want = ""
		}
		if config.DiagnosticTransferID != want || config.DiagnosticPortMap != (want != "") {
			t.Fatalf("unexpected RTC diagnostic scope for %s: %q, port map %v", device.ID, config.DiagnosticTransferID, config.DiagnosticPortMap)
		}
		config = peer.RTCConfig{} // JSON omits the field when disabled.
	}
	diagnostic.GatewayMapping = "private"
	do("PUT", aRTC+diagnosticPath, a.Token, diagnostic, 204, nil)
	do("PUT", bRTC+diagnosticPath, b.Token, diagnostic, 204, nil)
	do("PUT", cRTC+diagnosticPath, c.Token, diagnostic, 404, nil)
	do("PUT", aRTC+diagnosticPath, b.Token, diagnostic, 401, nil)
	diagnostic.AttemptID = strings.Repeat("b", 32)
	do("PUT", aRTC+diagnosticPath, a.Token, diagnostic, 404, nil)
	diagnostic.Stage = "arbitrary text"
	do("PUT", aRTC+diagnosticPath, a.Token, diagnostic, 400, nil)
	var answered peer.RTCAnswer
	query := "?attempt_id=" + signal.AttemptID
	do("GET", cRTC+answer+query, c.Token, nil, 404, nil)
	do("GET", aRTC+answer+query, a.Token, nil, 200, &answered)
	if answered.SDP != signal.SDP {
		t.Fatalf("sender did not get the matching answer: %+v", answered)
	}
	newSignal := signal
	newSignal.AttemptID = strings.Repeat("b", 32)
	do("PUT", offer, a.Token, newSignal, 204, nil)
	do("PUT", bRTC+answer, b.Token, signal, 409, nil)
	do("GET", aRTC+answer+query, a.Token, nil, 404, nil)
	delegation := workspace.Delegation{
		Generation: parent.Generation, Lease: parent.Lease, RequestKey: "write-description",
		ProjectID: project, BindingID: office, Title: "Write description",
		PromptTransferID: prompt.ID, TransferIDs: []string{file.ID},
	}
	var child, replay workspace.Task
	do("POST", runPath(a, parent)+"/children", a.Token, delegation, 202, &child)
	do("POST", runPath(a, parent)+"/children", a.Token, delegation, 202, &replay)
	if child.ID != replay.ID || child.ParentTaskID != parentID || child.RootTaskID != parentID ||
		child.Status != "waiting_delivery" {
		t.Fatalf("unexpected child and replay: %+v, %+v", child, replay)
	}
	conflict := delegation
	conflict.Title = "Changed title"
	do("POST", runPath(a, parent)+"/children", a.Token, conflict, 409, nil)
	waitProof := workspace.RunEvent{
		Generation: parent.Generation, Lease: parent.Lease, Kind: "waiting", WaitForTaskIDs: []string{parallelID},
	}
	do("POST", runPath(a, parent)+"/wait/validate", a.Token, waitProof, 409, nil)
	waitProof.WaitForTaskIDs = []string{child.ID}
	do("POST", runPath(a, parent)+"/wait/validate", a.Token, waitProof, 204, nil)
	event(a, parent, 3, workspace.RunEvent{
		Kind: "waiting", SessionID: "parent-native-session", WaitForTaskIDs: []string{child.ID},
	}, 204)

	independentID := createRoot(22, home)
	independent := claim(a)
	if independent.TaskID != independentID {
		t.Fatalf("parent did not release the home execution slot: %+v", independent)
	}
	event(a, independent, 1, workspace.RunEvent{Kind: "completed", Text: "Independent work done"}, 204)
	event(b, parallel, 2, workspace.RunEvent{Kind: "completed", Text: "Parallel work done"}, 204)

	prompt = authorize(a, prompt)
	do("POST", "devices/"+b.ID+"/transfers/"+prompt.ID+"/verify", a.Token,
		map[string]string{"ticket": prompt.Ticket}, 401, nil)
	ack(b, prompt)
	var detail workspace.Detail
	do("GET", "tasks/"+child.ID, testAdmin, nil, 200, &detail)
	if detail.Task.Status != "waiting_delivery" {
		t.Fatalf("child available before file ACK: %+v", detail.Task)
	}
	ack(b, authorize(a, file))
	run := claim(b)
	if run.TaskID != child.ID || run.Prompt != "" || run.PromptTransferID != prompt.ID || run.ParentDeviceID != a.ID {
		t.Fatalf("child received incorrect prompt routing: %+v", run)
	}
	event(b, run, 1, workspace.RunEvent{Kind: "started"}, 204)
	result := makeTransfer(b, a, run, 32, "result")
	event(b, run, 2, workspace.RunEvent{Kind: "completed", TransferIDs: []string{result.ID}}, 204)
	do("GET", "tasks/"+parentID, testAdmin, nil, 200, &detail)
	if detail.Task.Status != "waiting_dependencies" {
		t.Fatalf("parent woke before result ACK: %+v", detail.Task)
	}
	ack(a, authorize(b, result))

	resume := claim(a)
	if resume.TaskID != parentID || resume.ID == parent.ID || resume.SessionID != "parent-native-session" ||
		resume.Generation <= parent.Generation || len(resume.Attachments) != 1 || resume.Attachments[0].ID != result.ID {
		t.Fatalf("parent did not resume with the confirmed result: %+v", resume)
	}
	event(a, resume, 1, workspace.RunEvent{Kind: "completed", Text: "Index and description ready"}, 204)
	for range 3 {
		report := claimDelivery()
		if report.Kind != "report" || report.TaskID == child.ID {
			t.Fatalf("unexpected conversation delivery: %+v", report)
		}
		for range 2 {
			do("PUT", "plugin/deliveries/"+report.ID+"/complete", testPlugin, workspace.Completion{
				Lease: report.Lease, Status: "handled", Text: "Done",
			}, 204, nil)
		}
	}
	do("POST", "plugin/deliveries/claim", testPlugin, nil, 204, nil)
	standaloneID := createRoot(23, home)
	standalone := claim(a)
	if standalone.TaskID != standaloneID {
		t.Fatalf("unexpected standalone task: %+v", standalone)
	}
	event(a, standalone, 1, workspace.RunEvent{Kind: "started"}, 204)
	event(a, standalone, 2, workspace.RunEvent{Kind: "session", SessionID: "standalone-session"}, 204)
	waitedTransfer := makeTransfer(a, b, standalone, 33, "file")
	event(a, standalone, 3, workspace.RunEvent{Kind: "waiting", TransferIDs: []string{waitedTransfer.ID}}, 204)
	do("GET", "tasks/"+standaloneID, testAdmin, nil, 200, &detail)
	if detail.Task.Status != "waiting_delivery" {
		t.Fatalf("standalone transfer wait not visible: %+v", detail.Task)
	}
	ack(b, authorize(a, waitedTransfer))
	resumedTransferWait := claim(a)
	if resumedTransferWait.TaskID != standaloneID || resumedTransferWait.ID == standalone.ID ||
		resumedTransferWait.SessionID != "standalone-session" {
		t.Fatalf("standalone transfer wait did not resume: %+v", resumedTransferWait)
	}
	event(a, resumedTransferWait, 1, workspace.RunEvent{Kind: "completed", Text: "Transfer received"}, 204)
	lastReport := claimDelivery()
	if lastReport.Kind != "report" || lastReport.TaskID == child.ID {
		t.Fatalf("unexpected final root report: %+v", lastReport)
	}
	do("PUT", "plugin/deliveries/"+lastReport.ID+"/complete", testPlugin, workspace.Completion{
		Lease: lastReport.Lease, Status: "handled", Text: "Transfer received.",
	}, 204, nil)
	do("GET", "tasks/"+parentID, testAdmin, nil, 200, &detail)
	if len(detail.Runs) != 2 || len(detail.Children) != 1 || detail.Task.ReportPending {
		t.Fatalf("unexpected final task detail: %+v", detail)
	}
}
