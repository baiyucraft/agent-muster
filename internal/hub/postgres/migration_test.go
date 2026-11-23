package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/jackc/pgx/v5"
)

func newTestSchema(t *testing.T) (context.Context, string) {
	t.Helper()
	dsn := os.Getenv("MUSTER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MUSTER_TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	setup, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { setup.Close(context.Background()) })
	schemaName := fmt.Sprintf("muster_migration_%d", time.Now().UnixNano())
	schema := pgx.Identifier{schemaName}.Sanitize()
	if _, err := setup.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = setup.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	address, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := address.Query()
	query.Set("search_path", schemaName)
	address.RawQuery = query.Encode()
	return ctx, address.String()
}

func TestM0WorkspaceMigration(t *testing.T) {
	ctx, address := newTestSchema(t)
	old, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close(context.Background())
	for _, sql := range []string{createInventoryTables, createWorkspaceTables} {
		if _, err := old.Exec(ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatal(err)
		}
	}
	var token [32]byte
	token[0] = 1
	if _, err := old.Exec(ctx, `INSERT INTO muster_devices(device_id,token_hash,name,hostname,os,arch)
		VALUES ('device-a',$1,'Home','home','windows','amd64')`, token[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(ctx, `
		INSERT INTO muster_directories(device_id,directory_id,label) VALUES ('device-a','directory-a','Folder');
		INSERT INTO muster_projects(id,name,device_id,directory_id,directory_label)
			VALUES ('project-a','Project','device-a','directory-a','Folder');
		INSERT INTO muster_tasks(id,project_id,project_name,device_id,directory_id,title,status,current_run_id,session_id)
			VALUES ('task-test','project-a','Project','device-a','directory-a','Task','running','run-test','native');
		INSERT INTO muster_runs(id,task_id,input,status,claimed_at,input_session,session_id)
			VALUES ('run-test','task-test','original prompt','running',clock_timestamp(),'native','native');
		INSERT INTO muster_run_events(run_id,sequence,payload)
			VALUES ('run-test',1,'{"kind":"started"}');
	`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}

	store, err := OpenInventory(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Workspace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].RootTaskID != "task-test" ||
		snapshot.Tasks[0].BindingID != "project-a" || snapshot.Tasks[0].AgentKind != "dsh" ||
		len(snapshot.Projects) != 1 || len(snapshot.Projects[0].Bindings) != 1 ||
		!snapshot.Projects[0].Bindings[0].IsDefault {
		t.Fatalf("M0 settings did not migrate: %+v", snapshot)
	}
	claim, err := store.ClaimRun(ctx, "device-a", token)
	if err != nil {
		t.Fatal(err)
	}
	if claim.ID != "run-test" || claim.SessionID != "native" || claim.Prompt != "original prompt" ||
		claim.Generation != 1 || claim.Lease == "" || claim.AgentKind != "dsh" {
		t.Fatalf("M0 run did not reconcile: %+v", claim)
	}
	if err := store.RecordRunEvent(ctx, "device-a", token, claim.ID, 1, workspace.RunEvent{Kind: "started"}); err != nil {
		t.Fatalf("M0 pending receipt replay rejected: %v", err)
	}
	if _, err := old.Exec(ctx, "UPDATE muster_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", claim.ID); err != nil {
		t.Fatal(err)
	}
	bad := workspace.RunEvent{Kind: "completed", Text: "forged", Lease: claim.Lease, Generation: claim.Generation + 1}
	if err := store.RecordRunEvent(ctx, "device-a", token, claim.ID, 2, bad); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("different generation accepted: %v", err)
	}
	bad.Generation = claim.Generation
	bad.Lease = "different"
	if err := store.RecordRunEvent(ctx, "device-a", token, claim.ID, 2, bad); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("different lease accepted: %v", err)
	}
	if err := store.RecordRunEvent(ctx, "device-a", token, claim.ID, 2, workspace.RunEvent{
		Kind: "completed", Text: "recovered result", Lease: claim.Lease, Generation: claim.Generation,
	}); err != nil {
		t.Fatalf("owning receipt failed after lease expiry: %v", err)
	}
	reopened, err := OpenInventory(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	detail, err := reopened.TaskDetail(ctx, claim.TaskID)
	if err != nil || detail.Task.Status != "completed" || !detail.Task.ReportPending || len(detail.Runs) != 1 {
		t.Fatalf("migrated completion did not persist: %+v, %v", detail, err)
	}
	if detail.Runs[0].Diagnostics != nil {
		t.Fatalf("legacy run has synthetic diagnostics: %+v", detail.Runs[0].Diagnostics)
	}
	if detail.Task.CurrentRunGeneration != claim.Generation || len(detail.Inputs) != 1 ||
		detail.Inputs[0].Kind != "execution" || detail.Inputs[0].RunID != claim.ID ||
		detail.Inputs[0].Status != "completed" || detail.Inputs[0].ClaimedAt == nil {
		t.Fatalf("migrated input metadata did not persist: %+v", detail.Inputs)
	}
	if _, err := reopened.SubmitMessage(ctx, workspace.Message{
		ID: "follow-up", Text: "Update the result", TaskID: claim.TaskID,
	}); err != nil {
		t.Fatal(err)
	}
	detail, err = reopened.TaskDetail(ctx, claim.TaskID)
	if err != nil || len(detail.Inputs) != 2 || detail.Inputs[1].Kind != "message" ||
		detail.Inputs[1].MessageID != "follow-up" || detail.Inputs[1].Status != "accepted" ||
		detail.Inputs[1].RunID != "" || detail.Inputs[1].ClaimedAt != nil {
		t.Fatalf("queued follow-up metadata is incorrect: %+v, %v", detail.Inputs, err)
	}
}
