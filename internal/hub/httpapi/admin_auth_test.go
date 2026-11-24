package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baiyucraft/agent-muster/internal/hub/adminauth"
	"github.com/baiyucraft/agent-muster/internal/hub/postgres"
)

func TestAdminSessionAPI(t *testing.T) {
	ctx, _, _, address := newWorkspaceTestSchema(t)
	store, err := postgres.OpenInventory(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	handler := NewHandler(store, "", nil, WorkspaceOptions{
		Store: store, PluginToken: testPlugin, AdminAuth: AdminAuthOptions{Store: store, SecureCookie: true},
	})
	const (
		validLogin = `{"username":"owner","password":" fixture test password "}`
		badLogin   = `{"username":"owner","password":"incorrect password"}`
	)
	call := func(method, route, body, bearer, origin string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequestWithContext(ctx, method, "/api/v1/"+route, strings.NewReader(body))
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		recorded := httptest.NewRecorder()
		handler.ServeHTTP(recorded, request)
		if recorded.Code != want {
			t.Fatalf("%s %s status = %d, want %d (%s)", method, route, recorded.Code, want, recorded.Body.String())
		}
		return recorded
	}
	call("POST", "auth/login", validLogin, "", "https://foreign.example", nil, 403)
	call("POST", "auth/login", validLogin, "", "", nil, 503)
	hash, err := adminauth.HashPassword(" fixture test password ")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAdminCredentials(ctx, "owner", hash); err != nil {
		t.Fatal(err)
	}
	call("POST", "auth/login", badLogin, "", "", nil, 401)
	loggedIn := call("POST", "auth/login", validLogin, "", "https://example.com", nil, 204)
	cookies := loggedIn.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected one session cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "muster_admin_session" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode ||
		cookie.Path != "/api/v1" || cookie.Domain != "" || cookie.MaxAge != 86400 || cookie.Expires.IsZero() {
		t.Fatal("invalid session cookie attributes")
	}
	call("GET", "auth/session", "", "", "", cookie, 204)
	call("GET", "workspace", "", "", "", cookie, 200)
	call("GET", "devices", "", testPlugin, "", cookie, 401)
	call("GET", "plugin/projects", "", "", "", cookie, 401)
	call("POST", "pairing-codes", `{"name":"test device"}`, "", "https://foreign.example", cookie, 403)
	pairing := call("POST", "pairing-codes", `{"name":"test device"}`, "", "https://example.com", cookie, 201)
	var code struct{ Code string }
	if err := json.Unmarshal(pairing.Body.Bytes(), &code); err != nil {
		t.Fatal(err)
	}
	enrollment := call("POST", "devices/enroll", `{"code":"`+code.Code+`","hostname":"fixture","os":"windows","arch":"amd64"}`, "", "", nil, 201)
	var device struct {
		ID    string `json:"device_id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(enrollment.Body.Bytes(), &device); err != nil {
		t.Fatal(err)
	}
	heartbeat := "devices/" + device.ID + "/heartbeat"
	call("POST", heartbeat, "", "", "", cookie, 401)
	call("GET", "devices", "", device.Token, "", nil, 401)
	call("POST", "auth/logout", "", "", "https://foreign.example", cookie, 403)
	call("GET", "auth/session", "", "", "", cookie, 204)
	logout := call("POST", "auth/logout", "", "", "https://example.com", cookie, 204)
	cleared := logout.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != cookie.Name || cleared[0].Path != cookie.Path || cleared[0].MaxAge >= 0 {
		t.Fatal("logout did not expire the session cookie")
	}
	call("GET", "auth/session", "", "", "", cookie, 401)
	call("GET", "workspace", "", "", "", cookie, 401)
	call("POST", heartbeat, "", device.Token, "", nil, 204)
	call("GET", "plugin/projects", "", testPlugin, "", nil, 200)
}

func TestAdminLoginThrottlesDirectSources(t *testing.T) {
	handler := NewHandler(&memoryStore{}, "", nil)
	login := func(address, forwarded string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"owner","password":"fixture password"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Forwarded-For", forwarded)
		request.RemoteAddr = address
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	for range loginBurst {
		if got := login("[2001:db8:1:2::1]:1234", "").Code; got != http.StatusServiceUnavailable {
			t.Fatalf("uninitialized login status = %d", got)
		}
	}
	limited := login("[2001:db8:1:2::2]:5678", "198.51.100.42")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "10" {
		t.Fatalf("same IPv6 network status = %d, retry after = %q", limited.Code, limited.Header().Get("Retry-After"))
	}
	if got := login("[2001:db8:1:3::1]:1234", "").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("independent IPv6 network status = %d", got)
	}
}
