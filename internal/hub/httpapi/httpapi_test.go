package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
)

type memoryStore struct {
	pairingName string
	pairingHash [32]byte
	deviceID    string
	deviceHash  [32]byte
	inventory   registry.Inventory
}

func (store *memoryStore) CreatePairing(_ context.Context, name string, codeHash [32]byte) (time.Time, error) {
	store.pairingName = name
	store.pairingHash = codeHash
	return time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), nil
}

func (store *memoryStore) Enroll(_ context.Context, id string, _ registry.Enrollment, codeHash [32]byte, tokenHash [32]byte) error {
	if store.pairingHash != codeHash {
		return registry.ErrInvalidPairing
	}
	store.deviceID = id
	store.deviceHash = tokenHash
	return nil
}

func (store *memoryStore) Heartbeat(context.Context, string, [32]byte) error { return nil }

func (store *memoryStore) Report(_ context.Context, _ string, _ [32]byte, inventory registry.Inventory) error {
	store.inventory = inventory
	return nil
}

func (store *memoryStore) List(context.Context) ([]registry.Device, error) {
	return []registry.Device{}, nil
}

func (store *memoryStore) Revoke(context.Context, string) error { return nil }

func TestPairingAndEnrollment(t *testing.T) {
	store := &memoryStore{}
	handler := NewHandler(store, "admin-token", nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/pairing-codes", strings.NewReader(`{"name":"Home PC"}`))
	request.Header.Set("Authorization", "Bearer admin-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || store.pairingName != "Home PC" {
		t.Fatalf("pairing response = %d, store name = %q, body = %s", response.Code, store.pairingName, response.Body.String())
	}
	var pairing struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &pairing); err != nil {
		t.Fatal(err)
	}
	if !validRandomSecret(pairing.Code, pairingBytes) {
		t.Fatalf("pairing code has invalid format: %q", pairing.Code)
	}

	enrollmentBody := `{"code":"` + pairing.Code + `","hostname":"home","os":"windows","arch":"amd64"}`
	request = httptest.NewRequest(http.MethodPost, "/api/v1/devices/enroll", strings.NewReader(enrollmentBody))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("enrollment response = %d, body = %s", response.Code, response.Body.String())
	}
	var enrollment struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &enrollment); err != nil {
		t.Fatal(err)
	}
	if !registry.ValidDeviceID(enrollment.DeviceID) || !validRandomSecret(enrollment.Token, tokenBytes) {
		t.Fatalf("enrollment returned invalid credentials: %#v", enrollment)
	}
	if store.deviceID != enrollment.DeviceID || store.deviceHash != sha256.Sum256([]byte(enrollment.Token)) {
		t.Fatal("store did not receive the enrolled ID and token digest")
	}
}

func TestInventoryRequiresStrictKnownAgentShape(t *testing.T) {
	store := &memoryStore{}
	handler := NewHandler(store, "admin-token", nil)
	token := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	request := httptest.NewRequest(http.MethodPut, "/api/v1/devices/d-0123456789abcdef0123456789abcdef/inventory", strings.NewReader(`{"agents":[{"id":"codex","version":"1.2.3","status":"supported","capabilities":{"prompt":true,"final_reply":true,"resume":true}}]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("inventory response = %d, body = %s", response.Code, response.Body.String())
	}
	if len(store.inventory.Agents) != 1 || store.inventory.Agents[0].ID != "codex" {
		t.Fatalf("stored inventory = %#v", store.inventory)
	}

	request = httptest.NewRequest(http.MethodPut, "/api/v1/devices/d-0123456789abcdef0123456789abcdef/inventory", strings.NewReader(`{"agents":[{"id":"other","version":"1.2.3","status":"supported","capabilities":{"prompt":true,"final_reply":true,"resume":true}}]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid agent response = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestAdminEndpointsRejectMissingOrInvalidBearer(t *testing.T) {
	handler := NewHandler(&memoryStore{}, "admin-token", nil)
	for _, authorization := range []string{"", "Bearer wrong-token", "Bearer admin-token extra"} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q returned %d", authorization, response.Code)
		}
	}
}
