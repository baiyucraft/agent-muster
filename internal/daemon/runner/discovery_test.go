package runner

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestDiscoverMarksAbsentCLIsMissing(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("PATH", directory)
	agents := Discover(context.Background())
	if len(agents) != 2 {
		t.Fatalf("got %d Agent entries", len(agents))
	}
	for _, agent := range agents {
		if agent.Status != "missing" || agent.Version != "" {
			t.Errorf("unexpected absent Agent record: %+v", agent)
		}
	}
}

func TestInspectCommandHonorsCallerTimeout(t *testing.T) {
	t.Setenv("MUSTER_RUN_PROBE_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := inspectCommand(ctx, os.Args[0], "-test.run=^TestProbeHelper$")
	if err == nil {
		t.Fatal("expected command to time out")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("probe timeout took %s", elapsed)
	}
}

func TestProbeHelper(t *testing.T) {
	if os.Getenv("MUSTER_RUN_PROBE_HELPER") != "1" {
		return
	}
	time.Sleep(time.Second)
}
