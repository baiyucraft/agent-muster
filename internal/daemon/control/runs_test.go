package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunProtocolAuthenticatesAndParsesNoWork(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+testIdentity().Token {
			t.Errorf("missing device authorization")
		}
		switch r.URL.Path {
		case "/api/v1/devices/d-0123456789abcdef0123456789abcdef/directories":
			var payload struct {
				Directories []Directory `json:"directories"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload.Directories) != 1 {
				t.Errorf("directories = %#v, err=%v", payload, err)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/devices/d-0123456789abcdef0123456789abcdef/runs/claim":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	identity := testIdentity()
	identity.HubURL = server.URL
	client, err := NewClient(identity.HubURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReportDirectories(context.Background(), identity, []Directory{{ID: "dir-123", Label: "project"}}); err != nil {
		t.Fatal(err)
	}
	run, err := client.ClaimRun(context.Background(), identity)
	if err != nil || run != nil || calls != 2 {
		t.Fatalf("run=%#v calls=%d err=%v", run, calls, err)
	}
}

func TestRunProtocolParsesAssignedRunAndReceipt(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(Run{ID: "run-1", TaskID: "task-1", DirectoryID: "dir-1", Prompt: "work"})
			return
		}
		var event RunEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil || event.Kind != "completed" {
			t.Errorf("event=%#v err=%v", event, err)
		}
		if r.URL.Path != "/api/v1/devices/d-0123456789abcdef0123456789abcdef/runs/run-1/events/2" {
			t.Errorf("unexpected event path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	identity := testIdentity()
	identity.HubURL = server.URL
	client, err := NewClient(identity.HubURL)
	if err != nil {
		t.Fatal(err)
	}
	run, err := client.ClaimRun(context.Background(), identity)
	if err != nil || run == nil || run.ID != "run-1" {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	if err := client.ReportRunEvent(context.Background(), identity, run.ID, 2, RunEvent{Kind: "completed", Text: "ok"}); err != nil || requests != 2 {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
}

func testIdentity() Identity {
	return Identity{DeviceID: "d-0123456789abcdef0123456789abcdef", Token: "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"}
}
