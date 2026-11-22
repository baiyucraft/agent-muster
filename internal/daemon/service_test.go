package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

type testReporter struct {
	heartbeatErr error
	heartbeats   int
	inventories  int
}

func (reporter *testReporter) Heartbeat(context.Context, control.Identity) error {
	reporter.heartbeats++
	return reporter.heartbeatErr
}

func (reporter *testReporter) ReportInventory(context.Context, control.Identity, []runner.Agent) error {
	reporter.inventories++
	return nil
}

func TestRunReportsHeartbeatAndInventoryEvents(t *testing.T) {
	reporter := &testReporter{}
	events := make(chan Event, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, reporter, control.Identity{}, func(context.Context) []runner.Agent {
			return []runner.Agent{{ID: "codex"}}
		}, func(event Event) { events <- event })
	}()

	seen := map[string]Event{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case event := <-events:
			seen[event.Kind] = event
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for initial device reports")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if seen[EventHeartbeat].At.IsZero() {
		t.Fatal("heartbeat event did not include its observation time")
	}
	inventory := seen[EventInventory]
	if inventory.At.IsZero() || len(inventory.Agents) != 1 || inventory.Agents[0].ID != "codex" {
		t.Fatalf("unexpected inventory event: %+v", inventory)
	}
}

func TestRunStopsWhenDeviceCredentialIsRevoked(t *testing.T) {
	reporter := &testReporter{heartbeatErr: control.ErrUnauthorized}
	events := make(chan Event, 2)
	err := run(context.Background(), reporter, control.Identity{}, func(context.Context) []runner.Agent { return nil }, func(event Event) { events <- event })
	if !errors.Is(err, control.ErrUnauthorized) {
		t.Fatalf("run error = %v, want ErrUnauthorized", err)
	}
	for {
		select {
		case event := <-events:
			if event.Kind == EventRevoked {
				if event.Source != "heartbeat" || !errors.Is(event.Err, control.ErrUnauthorized) {
					t.Fatalf("unexpected revoked event: %+v", event)
				}
				return
			}
		default:
			t.Fatal("missing revoked event")
		}
	}
}
