package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/hub/postgres"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/inputreceipt"
	"github.com/jackc/pgx/v5"
)

type workspaceRequest func(method, path, token string, payload any, want int, output any)

const testAdmin = "m0-integration-admin-token-separate"
const testPlugin = "m0-integration-plugin-token-separate"

// The integration DSN points to a test database. Each invocation uses a fresh schema.
func openWorkspaceFixture(t *testing.T) (context.Context, *pgx.Conn, string, *httptest.Server, workspaceRequest) {
	t.Helper()
	dsn := os.Getenv("MUSTER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MUSTER_TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	schema := pgx.Identifier{fmt.Sprintf("m0_test_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	address, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := address.Query()
	query.Set("search_path", strings.Trim(schema, `"`))
	address.RawQuery = query.Encode()
	database, err := postgres.OpenInventory(ctx, address.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)

	server := httptest.NewServer(NewHandler(database, testAdmin, nil, WorkspaceOptions{Store: database, PluginToken: testPlugin}))
	t.Cleanup(server.Close)
	do := func(method, path, token string, payload any, want int, output any) {
		t.Helper()
		var body io.Reader
		if payload != nil {
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(data)
		}
		request, err := http.NewRequestWithContext(ctx, method, server.URL+"/api/v1/"+path, body)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			content, _ := io.ReadAll(response.Body)
			t.Fatalf("%s %s: got %d want %d, body=%s", method, path, response.StatusCode, want, content)
		}
		if output != nil {
			if err := json.NewDecoder(response.Body).Decode(output); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ctx, conn, schema, server, do
}

func TestWorkspaceRecovery(t *testing.T) {
	ctx, conn, schema, server, do := openWorkspaceFixture(t)
	const admin, plugin = testAdmin, testPlugin

	var pairing struct{ Code string }
	do("POST", "pairing-codes", admin, map[string]string{"name": "Test device"}, 201, &pairing)
	var device struct {
		ID    string `json:"device_id"`
		Token string
	}
	do("POST", "devices/enroll", "", map[string]string{"code": pairing.Code, "hostname": "test", "os": "windows", "arch": "amd64"}, 201, &device)
	prefix := "devices/" + device.ID + "/"
	dir := "dir-" + strings.Repeat("a", 32)
	do("PUT", prefix+"directories", device.Token, map[string]any{"directories": []workspace.Directory{{ID: dir, Label: "Temporary folder"}}}, 204, nil)
	do("GET", "workspace", plugin, nil, 401, nil)
	do("POST", "plugin/deliveries/claim", device.Token, nil, 401, nil)
	do("PUT", prefix+"directories", plugin, map[string]any{"directories": []workspace.Directory{}}, 401, nil)
	const project = "00000000-0000-4000-8000-000000000001"
	do("PUT", "projects/"+project, admin, map[string]string{"name": "Test Project", "device_id": device.ID, "directory_id": dir}, 200, nil)
	do("GET", "plugin/projects", plugin, nil, 200, nil)

	post := func(id, value, task string) {
		t.Helper()
		do("POST", "conversation/messages", admin, map[string]string{"id": id, "text": value, "task_id": task}, 202, nil)
	}
	claimDelivery := func(kind string) workspace.Delivery {
		t.Helper()
		var delivery workspace.Delivery
		do("POST", "plugin/deliveries/claim", plugin, nil, 200, &delivery)
		if delivery.Kind != kind {
			t.Fatalf("delivery kind %s, want %s", delivery.Kind, kind)
		}
		return delivery
	}
	complete := func(delivery workspace.Delivery, text string) {
		t.Helper()
		payload := workspace.Completion{Lease: delivery.Lease, Text: text, Status: "handled"}
		for range 2 {
			do("PUT", "plugin/deliveries/"+delivery.ID+"/complete", plugin, payload, 204, nil)
		}
	}
	post("00000000-0000-4000-8000-000000000009", "Interrupted center request", "")
	interrupted := claimDelivery("message")
	do("PUT", "plugin/deliveries/"+interrupted.ID+"/complete", plugin,
		workspace.Completion{Lease: interrupted.Lease, Status: "interrupted"}, 204, nil)
	if _, err := conn.Exec(ctx, "UPDATE "+schema+".muster_deliveries SET available_at=clock_timestamp() WHERE id=$1", interrupted.ID); err != nil {
		t.Fatal(err)
	}
	retry := claimDelivery("message")
	if retry.ID != interrupted.ID || retry.Attempt != interrupted.Attempt+1 {
		t.Fatalf("interrupted message was not requeued: %#v, %#v", interrupted, retry)
	}
	complete(retry, "Recovered response.")
	dispatch := func(delivery workspace.Delivery, input workspace.Dispatch) string {
		t.Helper()
		var response struct {
			TaskID string `json:"task_id"`
		}
		input.Lease = delivery.Lease
		do("POST", "plugin/deliveries/"+delivery.ID+"/dispatch", plugin, input, 200, &response)
		return response.TaskID
	}
	claimRun := func() workspace.RunClaim {
		t.Helper()
		var run workspace.RunClaim
		do("POST", prefix+"runs/claim", device.Token, nil, 200, &run)
		return run
	}
	record := func(run workspace.RunClaim, sequence int, event workspace.RunEvent, code int) {
		t.Helper()
		event.Generation, event.Lease = run.Generation, run.Lease
		do("PUT", fmt.Sprintf("%sruns/%s/events/%d", prefix, run.ID, sequence), device.Token, event, code, nil)
	}

	const message = "00000000-0000-4000-8000-000000000002"
	post(message, "List files", "")
	post(message, "List files", "")
	do("POST", "conversation/messages", admin, map[string]string{"id": message, "text": "Conflicting content"}, 409, nil)
	initial := claimDelivery("message")
	input := workspace.Dispatch{ProjectID: project, Title: "List files", Prompt: "List files"}
	task := dispatch(initial, input)
	if replay := dispatch(initial, input); replay != task {
		t.Fatalf("duplicate task %s != %s", replay, task)
	}
	complete(initial, "This dispatch acknowledgement stays on the user message.")
	run := claimRun()
	record(run, 1, workspace.RunEvent{Kind: "started"}, 204)
	record(run, 2, workspace.RunEvent{Kind: "session", SessionID: "session-test"}, 204)
	if replay := claimRun(); replay.ID != run.ID || replay.Prompt != run.Prompt ||
		replay.SessionID != run.SessionID || replay.Generation != run.Generation || replay.Lease != run.Lease {
		t.Fatalf("replayed claim changed: %#v, want %#v", replay, run)
	}
	post("00000000-0000-4000-8000-000000000003", "While device is busy", "")
	complete(claimDelivery("message"), "Accepted while active.")

	end := workspace.RunEvent{Kind: "completed", SessionID: "session-test", Text: "Created index.txt"}
	record(run, 3, end, 204)
	record(run, 3, end, 204)
	record(run, 3, workspace.RunEvent{Kind: "failed"}, 409)
	firstReport := claimDelivery("report")
	// Expire the claim to model a plugin process that died before acknowledging it.
	if _, err := conn.Exec(ctx, "UPDATE "+schema+".muster_deliveries SET lease_until=clock_timestamp() WHERE id=$1", firstReport.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed := claimDelivery("report")
	if reclaimed.ID != firstReport.ID || reclaimed.Lease == firstReport.Lease {
		t.Fatalf("report lease was not reclaimed: %#v, %#v", firstReport, reclaimed)
	}
	do("PUT", "plugin/deliveries/"+firstReport.ID+"/complete", plugin,
		workspace.Completion{Lease: firstReport.Lease, Status: "handled", Text: "Old lease"}, 409, nil)
	complete(reclaimed, "Index ready.")

	const amendmentID = "00000000-0000-4000-8000-000000000004"
	post(amendmentID, "Use CSV", task)
	followUp := claimDelivery("message")
	if updated := dispatch(followUp, workspace.Dispatch{TaskID: task, Prompt: "Use CSV"}); updated != task {
		t.Fatalf("follow-up created a different task %s", updated)
	}
	complete(followUp, "")
	client, err := control.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	deviceIdentity := control.Identity{HubURL: server.URL, DeviceID: device.ID, Token: device.Token}
	claimed, err := client.ClaimRun(ctx, deviceIdentity)
	if err != nil || claimed == nil {
		t.Fatalf("设备领取追加要求失败: %v", err)
	}
	next := workspace.RunClaim{ID: claimed.ID, SessionID: claimed.SessionID, DirectoryID: claimed.DirectoryID,
		Generation: claimed.Generation, Lease: claimed.Lease}
	if next.ID == run.ID || next.SessionID != "session-test" || next.DirectoryID != dir {
		t.Fatalf("wrong resume claim %#v", next)
	}
	if wanted := "message:" + amendmentID; len(claimed.InputIDs) != 1 || claimed.InputIDs[0] != wanted {
		t.Fatalf("领取结果未冻结追加输入: %v", claimed.InputIDs)
	}
	record(run, 4, workspace.RunEvent{Kind: "interrupted"}, 204)
	record(next, 1, workspace.RunEvent{Kind: "started", SessionID: next.SessionID}, 204)
	var preparedDetail workspace.Detail
	do("GET", "tasks/"+task, admin, nil, 200, &preparedDetail)
	receiptStatus := func(detail workspace.Detail) string {
		for _, input := range detail.Inputs {
			if input.MessageID == amendmentID {
				return input.ReceiptStatus
			}
		}
		return ""
	}
	if status := receiptStatus(preparedDetail); status != "delivered" {
		t.Fatalf("启动后追加输入状态为 %q", status)
	}
	completed := workspace.RunEvent{Kind: "completed", SessionID: next.SessionID, Text: "Created index.csv"}
	record(next, 2, completed, 400)
	completed.InputReceipt = &inputreceipt.Receipt{HandledIDs: []string{"message:unknown"}}
	record(next, 2, completed, 400)
	completed.InputReceipt = &inputreceipt.Receipt{HandledIDs: claimed.InputIDs}
	record(next, 2, completed, 204)
	record(next, 2, completed, 204)
	complete(claimDelivery("report"), "CSV ready.")

	var snapshot workspace.Snapshot
	do("GET", "workspace", admin, nil, 200, &snapshot)
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Status != "completed" || snapshot.Tasks[0].Result != "Created index.csv" ||
		snapshot.Tasks[0].ReportPending || len(snapshot.Messages) != 8 {
		t.Fatalf("unexpected snapshot %#v", snapshot)
	}
	var detail workspace.Detail
	do("GET", "tasks/"+task, admin, nil, 200, &detail)
	if len(detail.Runs) != 2 || detail.Runs[0].Status != "completed" || detail.Runs[1].SessionID != "session-test" {
		t.Fatalf("unexpected run history %#v", detail.Runs)
	}
	if status := receiptStatus(detail); status != "handled" {
		t.Fatalf("确认处理后追加输入状态为 %q", status)
	}
	do("GET", "events?after=99999", admin, nil, 409, nil)
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v1/events?after=%d", server.URL, snapshot.Cursor-2), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+admin)
	stream, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	lines := bufio.NewScanner(stream.Body)
	for cursor := snapshot.Cursor - 1; cursor <= snapshot.Cursor; cursor++ {
		found := false
		for lines.Scan() {
			if lines.Text() == fmt.Sprintf("id: %d", cursor) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing SSE cursor %d", cursor)
		}
	}
}
