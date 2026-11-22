package work

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
)

type delegationTestHub struct {
	collaborationHub
	requests  []control.Delegation
	published int
}

func (hub *delegationTestHub) CreateTransfer(_ context.Context, _ control.Identity, _ string, manifest control.Manifest) (control.Transfer, error) {
	hub.published++
	return control.Transfer{ID: manifest.ID}, nil
}

func (hub *delegationTestHub) Delegate(_ context.Context, _ control.Identity, _ string, request control.Delegation) (json.RawMessage, error) {
	hub.requests = append(hub.requests, request)
	if len(hub.requests) == 1 {
		return nil, errors.New("connection dropped after delegation")
	}
	return json.RawMessage(`{"id":"task-fixture"}`), nil
}

func TestDelegationRetriedAfterWorkerRestart(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	worker, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	hub := &delegationTestHub{}
	peer := &collaboration{worker: worker, client: hub, address: "configured"}
	run := control.Run{ID: "run-fixture", TaskID: "task-parent", Generation: 1, Lease: "fixture-lease"}
	if err := worker.saveClaim(ctx, run); err != nil {
		worker.Close()
		t.Fatal(err)
	}
	request := control.Delegation{
		LeaseProof: control.LeaseProof{Generation: run.Generation, Lease: run.Lease},
		RequestKey: "review", Title: "Review changes", PromptTransferID: transferKey(run.ID, "prompt", "review"),
	}
	if _, err := peer.stageDelegation(ctx, run, "device-child", request, []byte("review prompt")); err == nil {
		worker.Close()
		t.Fatal("expected the first attempt to lose its response")
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	worker, err = Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	peer.worker = worker
	if err := peer.flushDelegations(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := peer.stageDelegation(ctx, run, "device-child", request, []byte("review prompt"))
	if err != nil || string(result) != `{"id":"task-fixture"}` {
		t.Fatalf("saved delegation = %s, %v", result, err)
	}
	if hub.published != 1 || len(hub.requests) != 2 {
		t.Fatalf("published %d prompts and attempted %d requests", hub.published, len(hub.requests))
	}
	request.Title = "Different request"
	if _, err := peer.stageDelegation(ctx, run, "device-child", request, []byte("review prompt")); err == nil {
		t.Fatal("same key accepted a different request")
	}
}
