package runner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReadDSHPersistsSessionAndUsesFinalText(t *testing.T) {
	var stored string
	var response dshResponse
	err := response.read(strings.NewReader(
		`{"type":"session","sessionId":"session-123","cwd":"C:/work"}`+"\n"+
			`{"type":"status","phase":"step_start","turn":2,"step":1}`+"\n"+
			`{"type":"status","phase":"step_end","turn":2,"step":1,"usage":{"inputTokens":23,"outputTokens":4}}`+"\n"+
			`{"type":"final","text":"finished"}`+"\n"), "", func(id string) error {
		stored = id
		return nil
	})
	if err != nil || response.final != "finished" || stored != "session-123" {
		t.Fatalf("final=%q stored=%q err=%v", response.final, stored, err)
	}
	if report := response.usage.result(); report == nil || report.InputTokens != 23 || report.OutputTokens != 4 {
		t.Fatalf("用量 = %+v", report)
	}
}

func TestReadDSHRejectsSessionMismatch(t *testing.T) {
	var response dshResponse
	err := response.read(strings.NewReader("{\"type\":\"session\",\"sessionId\":\"session-other\"}\n"), "session-expected", func(string) error { return nil })
	if err == nil {
		t.Fatal("expected mismatched dsh session error")
	}
}

func TestReadDSHRetainsSessionWhenPersistenceFails(t *testing.T) {
	var response dshResponse
	err := response.read(strings.NewReader("{\"type\":\"session\",\"sessionId\":\"session-123\"}\n"), "", func(string) error {
		return errors.New("disk full")
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") || response.final != "" {
		t.Fatalf("final=%q err=%v", response.final, err)
	}
}

func TestDSHClassifiesOnlyUnopenedSession(t *testing.T) {
	const sessionID = "session-123"
	const busy = `session "session-123" is already owned by an active write handle`
	const missing = `session "session-123" does not exist; omit --session-id to start a new Session`
	for _, tc := range []struct {
		name, session, message, other string
		exitCode                      uint32
		want                          error
	}{
		{"busy", sessionID, busy, "", 1, ErrSessionBusy},
		{"unavailable", sessionID, missing, "", 1, ErrSessionUnavailable},
		{"corrupt", sessionID, `stored session "session-123" is corrupt: corrupt Zstandard session log: header frame failed validation`, "", 1, ErrSessionUnavailable},
		{"unsupported", sessionID, `failed to observe session "session-123": session "session-123" uses log format v99, but this harness reads only v4: the log was written by a newer harness — upgrade the harness to open it (raw log: C:\project\session.v99.jsonl.zstd)`, "", 1, ErrSessionUnavailable},
		{"first run", "", busy, "", 1, nil},
		{"other session", "session-456", busy, "", 1, nil},
		{"exit success", sessionID, busy, "", 0, nil},
		{"unexpected exit", sessionID, busy, "", 2, nil},
		{"unknown failure", sessionID, "operation timed out", "", 1, nil},
		{"read permission", sessionID, `failed to observe session "session-123": EACCES: permission denied`, "", 1, nil},
		{"session event", sessionID, busy, `{"type":"session","sessionId":"session-123"}`, 1, nil},
		{"structured output", sessionID, busy, `{"type":"status","phase":"step_start"}`, 1, nil},
		{"final response", sessionID, busy, `{"type":"final","text":"reply"}`, 1, nil},
		{"unknown stdout", sessionID, busy, `{"type":"unknown"}`, 1, nil},
		{"malformed stdout", sessionID, busy, `{"type":"session"`, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, err := json.Marshal(map[string]string{"type": "error", "message": tc.message})
			if err != nil {
				t.Fatal(err)
			}
			var response dshResponse
			if err := response.read(strings.NewReader(string(line)+"\n"+tc.other+"\n"), tc.session, func(string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			err = response.exitError(tc.session, tc.exitCode)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("分类 = %v，预期 %v", err, tc.want)
				}
			} else if err == nil || errors.Is(err, ErrSessionBusy) || errors.Is(err, ErrSessionUnavailable) {
				t.Fatalf("预期普通失败，得到 %v", err)
			}
		})
	}
}
