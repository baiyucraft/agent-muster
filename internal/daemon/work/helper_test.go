package work

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/helper"
)

type helperWaitHub struct {
	collaborationHub
	event control.RunEvent
	err   error
}

func (hub *helperWaitHub) ValidateWait(_ context.Context, _ control.Identity, _ string, event control.RunEvent) error {
	hub.event = event
	return hub.err
}

func TestHelperWaitRequiresHubValidationBeforePersisting(t *testing.T) {
	ctx := context.Background()
	worker, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	run := control.Run{ID: "run-test", TaskID: "task-parent", Generation: 2, Lease: "lease-test"}
	if err := worker.saveClaim(ctx, run); err != nil {
		t.Fatal(err)
	}
	hub := &helperWaitHub{err: &control.HTTPError{Status: http.StatusConflict}}
	service := &localHelper{
		peer:   &collaboration{worker: worker, client: hub},
		active: &activeRun{ctx: ctx, capability: "cap-test", run: run},
	}
	request := helper.Request{Command: "wait", Capability: "cap-test", TaskIDs: []string{"task-child"}}
	if _, err := service.handle(ctx, request); !errors.Is(err, hub.err) {
		t.Fatalf("rejected validation error = %v", err)
	}
	if waiting, err := worker.requestedWait(ctx, run.ID); err != nil || waiting.Kind != "" {
		t.Fatalf("rejected wait created a local record: %s, %v", waiting.Kind, err)
	}
	hub.err = nil
	if _, err := service.handle(ctx, request); err != nil {
		t.Fatal(err)
	}
	if hub.event.Generation != run.Generation || hub.event.Lease != run.Lease || len(hub.event.WaitForTaskIDs) != 1 {
		t.Fatal("wait validation did not include the active proof and dependency")
	}
	if waiting, err := worker.requestedWait(ctx, run.ID); err != nil || waiting.Kind != "waiting" || waiting.WaitForTaskIDs[0] != request.TaskIDs[0] {
		t.Fatalf("accepted wait did not persist the intent: %s, %v", waiting.Kind, err)
	}
}
