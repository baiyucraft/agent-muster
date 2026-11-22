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
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/peer"
)

const (
	turnTestDevice      = "d-0123456789abcdef0123456789abcdef"
	turnTestOtherDevice = "d-fedcba9876543210fedcba9876543210"
	turnTestSecret      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	turnTestURL         = "turn:relay.example.com:3478?transport=udp"
)

func testTURNRESTEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("MUSTER_RTC_STUN_URLS", "")
	t.Setenv("MUSTER_RTC_TURN_URLS", turnTestURL)
	t.Setenv("MUSTER_RTC_TURN_USERNAME", "")
	t.Setenv("MUSTER_RTC_TURN_CREDENTIAL", "")
	t.Setenv("MUSTER_RTC_TURN_REST_SECRET", turnTestSecret)
	t.Setenv("MUSTER_RTC_TURN_DEVICE_IDS", turnTestDevice)
}

func TestRTCTURNRESTCredential(t *testing.T) {
	testTURNRESTEnvironment(t)
	t.Setenv("MUSTER_RTC_TURN_DEVICE_IDS", turnTestOtherDevice+", "+turnTestDevice)
	config, err := configuredRTC(turnTestDevice, time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(config.ICEServers) != 1 {
		t.Fatal("试点设备应获得一个 TURN 服务")
	}
	server := config.ICEServers[0]
	if len(server.URLs) != 1 || server.URLs[0] != turnTestURL ||
		server.Username != "1800000600:"+turnTestDevice ||
		server.Credential != "VhqPGSop63Pu8QN6BxGxvCTuCHY=" {
		t.Fatal("TURN 凭据与预期的租期、设备或 Coturn 签名不一致")
	}
}

func TestRTCTURNModeValidation(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "missing URLs", key: "MUSTER_RTC_TURN_URLS"},
		{name: "invalid allowlist", key: "MUSTER_RTC_TURN_DEVICE_IDS", value: turnTestDevice + ",invalid"},
		{name: "missing allowlist", key: "MUSTER_RTC_TURN_DEVICE_IDS"},
		{name: "missing secret", key: "MUSTER_RTC_TURN_REST_SECRET"},
		{name: "short secret", key: "MUSTER_RTC_TURN_REST_SECRET", value: "ab"},
		{name: "wrong secret encoding", key: "MUSTER_RTC_TURN_REST_SECRET", value: strings.Repeat("z", 64)},
		{name: "mixed modes", key: "MUSTER_RTC_TURN_USERNAME", value: "legacy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			testTURNRESTEnvironment(t)
			t.Setenv(test.key, test.value)
			config, err := configuredRTC(turnTestDevice, time.Unix(1800000000, 0))
			if err == nil || len(config.ICEServers) != 0 || strings.Contains(err.Error(), turnTestSecret) {
				t.Fatal("无效的 TURN 试点配置应安全失败")
			}
		})
	}
}

func TestRTCTURNLegacyStaticCredentials(t *testing.T) {
	testTURNRESTEnvironment(t)
	t.Setenv("MUSTER_RTC_TURN_REST_SECRET", "")
	t.Setenv("MUSTER_RTC_TURN_DEVICE_IDS", "")
	t.Setenv("MUSTER_RTC_TURN_USERNAME", "legacy-device")
	t.Setenv("MUSTER_RTC_TURN_CREDENTIAL", "legacy-password")
	config, err := configuredRTC(turnTestOtherDevice, time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(config.ICEServers) != 1 || config.ICEServers[0].Username != "legacy-device" ||
		config.ICEServers[0].Credential != "legacy-password" {
		t.Fatal("原有静态 TURN 配置应继续生效")
	}
}

// turnTestWorkspace 用虚拟设备身份验证正常 HTTP 路由中的认证和发放顺序。
type turnTestWorkspace struct {
	workspace.Store
	rtcStore
	tokens map[string][32]byte
}

func (store turnTestWorkspace) RegisterRTC(_ context.Context, device string, token [32]byte) error {
	if expected, ok := store.tokens[device]; !ok || token != expected {
		return registry.ErrUnauthorized
	}
	return nil
}

func TestRTCTURNRESTRequiresDeviceAuthorization(t *testing.T) {
	testTURNRESTEnvironment(t)
	t.Setenv("MUSTER_RTC_STUN_URLS", "stun:stun.example.com:3478")
	allowedToken := strings.Repeat("A", 43)
	otherToken := strings.Repeat("Q", 43)
	store := turnTestWorkspace{tokens: map[string][32]byte{
		turnTestDevice:      sha256.Sum256([]byte(allowedToken)),
		turnTestOtherDevice: sha256.Sum256([]byte(otherToken)),
	}}
	handler := NewHandler(&memoryStore{}, "test-admin", nil, WorkspaceOptions{Store: store, PluginToken: "test-plugin"})
	for _, test := range []struct {
		name    string
		device  string
		token   string
		status  int
		servers int
	}{
		{name: "eligible device", device: turnTestDevice, token: allowedToken, status: http.StatusOK, servers: 2},
		{name: "other device keeps STUN", device: turnTestOtherDevice, token: otherToken, status: http.StatusOK, servers: 1},
		{name: "wrong device token", device: turnTestDevice, token: otherToken, status: http.StatusUnauthorized},
		{name: "missing token", device: turnTestDevice, status: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/api/v1/devices/"+test.device+"/rtc", nil)
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var config peer.RTCConfig
			if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.status || len(config.ICEServers) != test.servers ||
				response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("设备登记响应为 %d，ICE 服务数 %d", response.Code, len(config.ICEServers))
			}
			if strings.Contains(response.Body.String(), turnTestSecret) {
				t.Fatal("设备配置不得包含 TURN 服务端密钥")
			}
		})
	}
}
