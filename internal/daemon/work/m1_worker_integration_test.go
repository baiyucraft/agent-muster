//go:build windows

package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/helper"
	"github.com/baiyucraft/agent-muster/internal/hub/httpapi"
	"github.com/baiyucraft/agent-muster/internal/hub/postgres"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/inputreceipt"
	"github.com/jackc/pgx/v5"
)

const (
	workerTestAdmin  = "worker-integration-admin-token"
	workerTestPlugin = "worker-integration-plugin-token"
)

func TestM1WorkersHandoffAndResume(t *testing.T) {
	dsn := os.Getenv("MUSTER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MUSTER_TEST_DATABASE_URL is required")
	}
	t.Setenv("MUSTER_RTC_STUN_URLS", "")
	t.Setenv("MUSTER_RTC_TURN_URLS", "")
	t.Run("HTTPS", func(t *testing.T) { testM1WorkerHandoff(t, dsn, true) })
	t.Run("WebRTC", func(t *testing.T) { testM1WorkerHandoff(t, dsn, false) })
}

func testM1WorkerHandoff(t *testing.T, dsn string, useHTTPS bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	store, server := workerTestHub(t, ctx, dsn)
	client, err := control.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	parent, parentID, parentDir := workerTestDevice(t, ctx, server, client, "parent")
	child, childID, childDir := workerTestDevice(t, ctx, server, client, "child")
	const projectID = "10000000-0000-4000-8000-000000000001"
	const childBinding = "10000000-0000-4000-8000-000000000003"
	_, err = store.SaveProject(ctx, workspace.Project{ID: projectID, Name: "Handoff", Bindings: []workspace.Binding{
		{ID: "10000000-0000-4000-8000-000000000002", DeviceID: parentID.DeviceID, DirectoryID: parentDir.ID, AgentKind: "dsh", IsDefault: true},
		{ID: childBinding, DeviceID: childID.DeviceID, DirectoryID: childDir.ID, AgentKind: "codex"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	const indexName = "index.csv"
	indexBody := "name,size\r\n" + strings.Repeat("material.png,42\r\n", 4096)
	const childPrompt = "Verify the delivered index."
	const childResult = "The received index matches the expected row."
	const parentSession = "worker-integration-parent-session"
	const finalResult = "Handoff accepted and parent session resumed."
	if err := os.WriteFile(filepath.Join(parentDir.Path, indexName), []byte(indexBody), 0600); err != nil {
		t.Fatal(err)
	}

	parentExec := func(ctx context.Context, kind, dir, prompt, session string, env []string, started func() error, recordSession func(string) error) (string, error) {
		if kind != "dsh" || dir != parentDir.Path {
			return "", fmt.Errorf("unexpected parent binding: %s %s", kind, dir)
		}
		if err := started(); err != nil {
			return "", err
		}
		var status struct {
			TaskID            string                     `json:"task_id"`
			RunID             string                     `json:"run_id"`
			ProjectID         string                     `json:"project_id"`
			DelegationTargets []control.DelegationTarget `json:"delegation_targets"`
			DependencyResults []control.DependencyResult `json:"dependency_results"`
		}
		if err := workerHelperCall(ctx, env, helper.Request{Command: "status"}, &status); err != nil {
			return "", fmt.Errorf("read current run status: %w", err)
		}
		if status.TaskID == "" || status.RunID == "" || status.ProjectID != projectID {
			return "", fmt.Errorf("unexpected helper status identity: %+v", status)
		}
		if session == parentSession {
			if len(status.DependencyResults) != 1 || status.DependencyResults[0].TaskID == "" || status.DependencyResults[0].Status != "completed" {
				return "", fmt.Errorf("unexpected dependency results: %+v", status.DependencyResults)
			}
			if !strings.Contains(prompt, "result.txt") {
				return "", errors.New("resumed parent prompt did not include the result attachment")
			}
			inputID, err := checkWorkerAttachment(dir, "result.txt", childResult)
			if err != nil {
				return "", err
			}
			if !strings.Contains(prompt, inputID) {
				return "", errors.New("parent did not receive the frozen result input ID")
			}
			return testWorkerReply(finalResult, inputID), nil
		}
		if session != "" {
			return "", fmt.Errorf("unexpected parent session: %q", session)
		}
		if err := recordSession(parentSession); err != nil {
			return "", err
		}
		var target control.DelegationTarget
		for _, candidate := range status.DelegationTargets {
			if candidate.BindingID == childBinding {
				target = candidate
				break
			}
		}
		if target.DeviceID != childID.DeviceID || target.AgentKind != "codex" {
			return "", fmt.Errorf("unexpected helper delegation target: %+v", target)
		}
		var attachment control.Transfer
		err := workerHelperCall(ctx, env, helper.Request{Command: "send", Key: "index", File: indexName, TargetDeviceID: target.DeviceID}, &attachment)
		if err != nil {
			return "", fmt.Errorf("send index: %w", err)
		}
		var delegated workspace.Task
		err = workerHelperCall(ctx, env, helper.Request{
			Command: "delegate", Key: "review", Title: "Check index", Prompt: childPrompt,
			BindingID: target.BindingID, TargetDeviceID: target.DeviceID, TransferIDs: []string{attachment.ID},
		}, &delegated)
		if err != nil {
			return "", fmt.Errorf("spawn child: %w", err)
		}
		if err := workerHelperCall(ctx, env, helper.Request{Command: "wait", TaskIDs: []string{delegated.ID}}, nil); err != nil {
			return "", fmt.Errorf("wait for child: %w", err)
		}
		return "Waiting for the child.", nil
	}

	childExec := func(ctx context.Context, kind, dir, prompt, session string, _ []string, started func() error, recordSession func(string) error) (string, error) {
		if kind != "codex" || dir != childDir.Path || session != "" {
			return "", fmt.Errorf("unexpected child binding or session: %s %s %q", kind, dir, session)
		}
		if err := started(); err != nil {
			return "", err
		}
		if err := recordSession("worker-integration-child-session"); err != nil {
			return "", err
		}
		if !strings.HasPrefix(prompt, childPrompt) || !strings.Contains(prompt, indexName) {
			return "", errors.New("child prompt did not include the delegated text and attachment")
		}
		inputID, err := checkWorkerAttachment(dir, indexName, indexBody)
		if err != nil {
			return "", err
		}
		if !strings.Contains(prompt, inputID) {
			return "", errors.New("child did not receive the frozen file input ID")
		}
		return testWorkerReply(childResult, inputID), nil
	}

	workerCtx, stopWorkers := context.WithCancel(ctx)
	var parentAddress, childAddress string
	if useHTTPS {
		parentAddress, childAddress = workerTestAddresses(t)
	}
	parentDone := startTestWorker(t, workerCtx, parent, parentID, client, parentAddress, parentExec)
	var childDone <-chan struct{}
	defer func() {
		stopWorkers()
		<-parentDone
		if childDone != nil {
			<-childDone
		}
	}()

	const messageID = "10000000-0000-4000-8000-000000000004"
	if _, err := store.SubmitMessage(ctx, workspace.Message{ID: messageID, Text: "Prepare a file and request review."}); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.ClaimDelivery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rootID, err := store.Dispatch(ctx, delivery.ID, workspace.Dispatch{
		Lease: delivery.Lease, ProjectID: projectID, Title: "Prepare index", Prompt: "Prepare the index.",
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting := awaitWorkerTask(t, ctx, store, rootID, "waiting_dependencies")
	if len(waiting.Children) != 1 || waiting.Children[0].Status != "waiting_delivery" ||
		len(waiting.Dependencies) != 1 || waiting.Dependencies[0].DependsOnTaskID != waiting.Children[0].ID {
		t.Fatalf("incorrect child dependency: children=%+v dependencies=%+v", waiting.Children, waiting.Dependencies)
	}
	childDone = startTestWorker(t, workerCtx, child, childID, client, childAddress, childExec)
	completed := awaitWorkerTask(t, ctx, store, rootID, "completed")
	if completed.Task.Result != finalResult || len(completed.Runs) != 2 || completed.Runs[0].SessionID != parentSession || completed.Runs[1].SessionID != parentSession {
		t.Fatalf("parent did not resume in the same session: task=%+v runs=%+v", completed.Task, completed.Runs)
	}
	if len(completed.Children) != 1 || completed.Children[0].Status != "completed" || completed.Children[0].Result != "" {
		t.Fatalf("child handoff metadata: %+v", completed.Children)
	}
	childDetail, err := store.TaskDetail(ctx, completed.Children[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	assertWorkerAttachmentHandled(t, completed, parentDir.Path, "result.txt", childResult)
	assertWorkerAttachmentHandled(t, childDetail, childDir.Path, indexName, indexBody)
	if len(completed.Transfers) != 3 {
		t.Fatalf("expected prompt, attachment and result deliveries: %+v", completed.Transfers)
	}
	for _, transfer := range completed.Transfers {
		if transfer.ReceivedAt == nil {
			t.Fatalf("transfer was not confirmed: %+v", transfer)
		}
	}
}

func workerTestHub(t *testing.T, ctx context.Context, dsn string) (*postgres.InventoryStore, *httptest.Server) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	schema := pgx.Identifier{fmt.Sprintf("m1_workers_%d", time.Now().UnixNano())}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(cleanup, "DROP SCHEMA "+schema.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	address, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := address.Query()
	query.Set("search_path", schema[0])
	address.RawQuery = query.Encode()
	store, err := postgres.OpenInventory(ctx, address.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	server := httptest.NewServer(httpapi.NewHandler(store, workerTestAdmin, nil, httpapi.WorkspaceOptions{
		Store: store, PluginToken: workerTestPlugin,
	}))
	t.Cleanup(server.Close)
	return store, server
}

func workerTestDevice(t *testing.T, ctx context.Context, server *httptest.Server, client *control.Client, name string) (*Worker, control.Identity, Directory) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/pairing-codes", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+workerTestAdmin)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("pairing %s: %s", name, response.Status)
	}
	var pairing struct{ Code string }
	if err := json.NewDecoder(response.Body).Decode(&pairing); err != nil {
		t.Fatal(err)
	}
	identity, err := client.Enroll(ctx, pairing.Code, name)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { worker.Close() })
	directory, err := worker.RegisterDirectory(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReportDirectories(ctx, identity, []control.Directory{{ID: directory.ID, Label: directory.Label}}); err != nil {
		t.Fatal(err)
	}
	return worker, identity, directory
}

func workerTestAddresses(t *testing.T) (string, string) {
	t.Helper()
	open := func() net.Listener {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return listener
	}
	first := open()
	defer first.Close()
	second := open()
	defer second.Close()
	return "https://" + first.Addr().String(), "https://" + second.Addr().String()
}

func startTestWorker(t *testing.T, ctx context.Context, worker *Worker, identity control.Identity, client *control.Client, address string, execute agentExecutor) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := worker.runWithPeer(ctx, client, identity, PeerConfig{Address: address}, func(err error) {
			if err != nil && ctx.Err() == nil {
				t.Logf("%s worker: %v", address, err)
			}
		}, execute)
		if err != nil && ctx.Err() == nil {
			t.Errorf("%s worker stopped: %v", address, err)
		}
	}()
	return done
}

func workerHelperCall(ctx context.Context, env []string, request helper.Request, output any) error {
	var pipe string
	for _, setting := range env {
		key, value, _ := strings.Cut(setting, "=")
		switch key {
		case "MUSTER_PIPE":
			pipe = value
		case "MUSTER_RUN_CAPABILITY":
			request.Capability = value
		}
	}
	if pipe == "" || request.Capability == "" {
		return errors.New("missing helper pipe or run capability")
	}
	payload, err := helper.Call(ctx, pipe, request)
	if err != nil || output == nil {
		return err
	}
	return json.Unmarshal(payload, output)
}

func checkWorkerAttachment(dir, name, want string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, ".muster-inbox", "*", name))
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected one %s attachment, found %d", name, len(matches))
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		return "", err
	}
	if string(body) != want {
		return "", fmt.Errorf("attachment %s contains %q, want %q", name, body, want)
	}
	return "transfer:" + filepath.Base(filepath.Dir(matches[0])), nil
}

func testWorkerReply(body string, handledIDs ...string) string {
	encoded, _ := json.Marshal(inputreceipt.Receipt{HandledIDs: handledIDs})
	return body + "\n" + inputReceiptMarker + "\n" + string(encoded)
}

func assertWorkerAttachmentHandled(t *testing.T, detail workspace.Detail, dir, name, contents string) {
	t.Helper()
	id, err := checkWorkerAttachment(dir, name, contents)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range detail.Inputs {
		if input.ID == id {
			if input.ReceiptStatus != "handled" || input.ReceiptRunID != input.RunID || input.HandledAt == nil {
				t.Fatalf("attachment input not confirmed by the handling run: %+v", input)
			}
			return
		}
	}
	t.Fatalf("no receipt record for attachment %s: %+v", id, detail.Inputs)
}

func awaitWorkerTask(t *testing.T, ctx context.Context, store *postgres.InventoryStore, id, status string) workspace.Detail {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		detail, err := store.TaskDetail(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		switch detail.Task.Status {
		case status:
			return detail
		case "failed", "interrupted":
			t.Fatalf("task %s before reaching %s: %+v", detail.Task.Status, status, detail)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("task did not reach %s: %+v", status, detail)
		case <-ticker.C:
		}
	}
}
