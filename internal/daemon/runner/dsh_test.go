package runner

import (
	"errors"
	"strings"
	"testing"
)

func TestReadDSHPersistsSessionAndUsesFinalText(t *testing.T) {
	var stored string
	final, err := readDSH(strings.NewReader("{\"type\":\"session\",\"sessionId\":\"session-123\",\"cwd\":\"C:/work\"}\n{\"type\":\"final\",\"text\":\"finished\"}\n"), "", func(id string) error {
		stored = id
		return nil
	})
	if err != nil || final != "finished" || stored != "session-123" {
		t.Fatalf("final=%q stored=%q err=%v", final, stored, err)
	}
}

func TestReadDSHRejectsSessionMismatch(t *testing.T) {
	_, err := readDSH(strings.NewReader("{\"type\":\"session\",\"sessionId\":\"session-other\"}\n"), "session-expected", func(string) error { return nil })
	if err == nil {
		t.Fatal("expected mismatched dsh session error")
	}
}

func TestReadDSHRetainsSessionWhenPersistenceFails(t *testing.T) {
	final, err := readDSH(strings.NewReader("{\"type\":\"session\",\"sessionId\":\"session-123\"}\n"), "", func(string) error {
		return errors.New("disk full")
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") || final != "" {
		t.Fatalf("final=%q err=%v", final, err)
	}
}
