package runner

import (
	"errors"
	"strings"
	"testing"
)

func TestReadCodexCapturesThreadAndCompletedFinal(t *testing.T) {
	var stored string
	result, err := readCodex(strings.NewReader(
		"{\"type\":\"thread.started\",\"thread_id\":\"550e8400-e29b-41d4-a716-446655440000\"}\n"+
			"{\"type\":\"turn.started\",\"turn_id\":\"turn-1\"}\n"+
			"{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}\n"+
			"{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":20,\"output_tokens\":3}}\n",
	), "", func(id string) error {
		stored = id
		return nil
	})
	if err != nil || result.SessionID != "550e8400-e29b-41d4-a716-446655440000" || result.Final != "done" || !result.Completed || stored != result.SessionID {
		t.Fatalf("result=%#v stored=%q err=%v", result, stored, err)
	}
	if result.Usage == nil || result.Usage.InputTokens != 20 || result.Usage.OutputTokens != 3 {
		t.Fatalf("用量 = %+v", result.Usage)
	}
}

func TestReadCodexRejectsUnexpectedResumedThread(t *testing.T) {
	_, err := readCodex(strings.NewReader("{\"type\":\"thread.started\",\"thread_id\":\"other\"}\n"), "expected", func(string) error { return nil })
	if err == nil {
		t.Fatal("expected a thread identity mismatch")
	}
}

func TestReadCodexFailsIfSessionCannotBePersisted(t *testing.T) {
	_, err := readCodex(strings.NewReader("{\"type\":\"thread.started\",\"thread_id\":\"550e8400-e29b-41d4-a716-446655440000\"}\n"), "", func(string) error {
		return errors.New("database unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestReadCodexDoesNotTreatFinalItemAsCompletedTurn(t *testing.T) {
	result, err := readCodex(strings.NewReader(
		"{\"type\":\"thread.started\",\"thread_id\":\"550e8400-e29b-41d4-a716-446655440000\"}\n"+
			"{\"type\":\"turn.started\"}\n"+
			"{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"partial\"}}\n"+
			"{\"type\":\"turn.failed\",\"error\":{\"message\":\"failed\"}}\n",
	), "", func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Completed || result.Failure != "failed" {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestFilteredAgentEnvironmentAllowsOnlyRunPipeCapability(t *testing.T) {
	got := filteredAgentEnvironment([]string{"PATH=C:\\Windows", "MUSTER_PIPE=inherited", "MUSTER_DEVICE_TOKEN=secret", "MUSTER_OTHER=x"}, []string{
		"MUSTER_PIPE=\\\\.\\pipe\\muster",
		"MUSTER_RUN_CAPABILITY=one-run-token",
		"MUSTER_DEVICE_TOKEN=leaked",
	})
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "MUSTER_PIPE=inherited") || !strings.Contains(joined, "MUSTER_PIPE=\\\\.\\pipe\\muster") || !strings.Contains(joined, "MUSTER_RUN_CAPABILITY=one-run-token") || strings.Contains(joined, "DEVICE_TOKEN") || strings.Contains(joined, "MUSTER_OTHER") {
		t.Fatalf("unexpected agent environment %q", joined)
	}
}
