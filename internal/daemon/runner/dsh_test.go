package runner

import (
	"errors"
	"strings"
	"testing"
)

func TestReadDSHPersistsSessionAndUsesFinalText(t *testing.T) {
	var stored string
	var usage dshUsage
	final, err := readDSH(strings.NewReader(
		`{"type":"session","sessionId":"session-123","cwd":"C:/work"}`+"\n"+
			`{"type":"status","phase":"step_start","turn":2,"step":1}`+"\n"+
			`{"type":"status","phase":"step_end","turn":2,"step":1,"usage":{"inputTokens":23,"outputTokens":4}}`+"\n"+
			`{"type":"final","text":"finished"}`+"\n"), "", func(id string) error {
		stored = id
		return nil
	}, &usage)
	if err != nil || final != "finished" || stored != "session-123" {
		t.Fatalf("final=%q stored=%q err=%v", final, stored, err)
	}
	if report := usage.result(); report == nil || report.InputTokens != 23 || report.OutputTokens != 4 {
		t.Fatalf("用量 = %+v", report)
	}
}

func TestReadDSHRejectsSessionMismatch(t *testing.T) {
	_, err := readDSH(strings.NewReader("{\"type\":\"session\",\"sessionId\":\"session-other\"}\n"), "session-expected", func(string) error { return nil }, new(dshUsage))
	if err == nil {
		t.Fatal("expected mismatched dsh session error")
	}
}

func TestReadDSHRetainsSessionWhenPersistenceFails(t *testing.T) {
	final, err := readDSH(strings.NewReader("{\"type\":\"session\",\"sessionId\":\"session-123\"}\n"), "", func(string) error {
		return errors.New("disk full")
	}, new(dshUsage))
	if err == nil || !strings.Contains(err.Error(), "disk full") || final != "" {
		t.Fatalf("final=%q err=%v", final, err)
	}
}
