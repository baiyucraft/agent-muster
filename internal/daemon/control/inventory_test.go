package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

func TestClientEnrollAndReport(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/devices/enroll":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
				t.Errorf("unexpected enrollment request: %s %s", r.Method, r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"device_id":"d-0123456789abcdef0123456789abcdef","token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`))
		case "/api/v1/devices/d-0123456789abcdef0123456789abcdef/heartbeat":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
				t.Errorf("unexpected heartbeat request: %s %s", r.Method, r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/devices/d-0123456789abcdef0123456789abcdef/inventory":
			if r.Method != http.MethodPut || r.Header.Get("Authorization") == "" {
				t.Errorf("unexpected inventory request: %s %s", r.Method, r.Header.Get("Authorization"))
			}
			var payload struct {
				Agents []runner.Agent `json:"agents"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload.Agents) != 1 || payload.Agents[0].ID != "codex" {
				t.Errorf("unexpected inventory payload: %+v (%v)", payload, err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := client.Enroll(context.Background(), "AAAAAAAAAAAAAAAAAAAAAAAA", "device")
	if err != nil {
		t.Fatal(err)
	}
	if identity.HubURL != server.URL {
		t.Fatalf("HubURL = %q", identity.HubURL)
	}
	if err := client.Heartbeat(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportInventory(context.Background(), identity, []runner.Agent{{ID: "codex"}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(requests, ","); got != "POST /api/v1/devices/enroll,POST /api/v1/devices/d-0123456789abcdef0123456789abcdef/heartbeat,PUT /api/v1/devices/d-0123456789abcdef0123456789abcdef/inventory" {
		t.Fatalf("request sequence = %s", got)
	}
}

func TestClientValidatesHubURL(t *testing.T) {
	for _, hubURL := range []string{"ftp://localhost", "https://user:password@example.com", "https://example.com/path?token=x"} {
		if _, err := NewClient(hubURL); err == nil {
			t.Errorf("NewClient(%q) succeeded", hubURL)
		}
	}
	for _, hubURL := range []string{"http://127.0.0.1:8080", "http://192.0.2.1:13011", "https://example.com"} {
		if _, err := NewClient(hubURL); err != nil {
			t.Errorf("NewClient(%q) failed: %v", hubURL, err)
		}
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Enroll(context.Background(), "AAAAAAAAAAAAAAAAAAAAAAAA", "device")
	if err == nil {
		t.Fatal("expected redirect to be rejected")
	}
	if redirected {
		t.Fatal("followed redirect and sent request to another host")
	}
}

func TestClientMapsUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{HubURL: server.URL, DeviceID: "d-0123456789abcdef0123456789abcdef", Token: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	if err := client.Heartbeat(context.Background(), identity); err != ErrUnauthorized {
		t.Fatalf("Heartbeat error = %v, want ErrUnauthorized", err)
	}
}
