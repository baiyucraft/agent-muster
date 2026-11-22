package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/peer"
)

const turnCredentialLifetime = 10 * time.Minute

// rtcStore 按设备身份和投递双方的关系校验信令的读写权限。
type rtcStore interface {
	RegisterRTC(context.Context, string, [32]byte) error
	RTCOffers(context.Context, string, [32]byte) ([]peer.RTCOffer, error)
	PutRTCOffer(context.Context, string, [32]byte, string, peer.RTCSignal) error
	RTCAnswer(context.Context, string, [32]byte, string, string) (peer.RTCAnswer, error)
	PutRTCAnswer(context.Context, string, [32]byte, string, peer.RTCSignal) error
}

type rtcDiagnosticStore interface {
	RTCDiagnosticRole(context.Context, string, [32]byte, string, string) (string, error)
}

func (api workspaceAPI) rtcStorage(w http.ResponseWriter) (rtcStore, bool) {
	store, ok := api.store.(rtcStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "device signaling unavailable")
	}
	return store, ok
}

func (api workspaceAPI) registerRTC(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	store, ok := api.rtcStorage(w)
	if !ok {
		return
	}
	if r.ContentLength != 0 {
		writeError(w, http.StatusBadRequest, "registration body must be empty")
		return
	}
	config, err := configuredRTC(device, time.Now())
	if err != nil {
		log.Printf("device ICE configuration invalid: %v", err)
		writeError(w, http.StatusServiceUnavailable, "invalid device ICE configuration")
		return
	}
	if err := store.RegisterRTC(r.Context(), device, token); err != nil {
		workspaceJSON(w, http.StatusOK, nil, err)
		return
	}
	if transfer := configuredRTCDiagnostic(); transfer != "" {
		if diagnostics, ok := store.(rtcDiagnosticStore); ok {
			if _, err := diagnostics.RTCDiagnosticRole(r.Context(), device, token, transfer, ""); err == nil {
				config.DiagnosticTransferID = transfer
				config.DiagnosticPortMap = os.Getenv("MUSTER_RTC_DIAGNOSTIC_PORT_MAPPING") == "1"
			}
		}
	}
	workspaceJSON(w, http.StatusOK, config, nil)
}

// configuredRTCDiagnostic 返回运营配置限定的单笔诊断投递。缺省关闭。
func configuredRTCDiagnostic() string {
	id := os.Getenv("MUSTER_RTC_DIAGNOSTIC_TRANSFER_ID")
	if prefixedUUID(id, "transfer-") {
		return id
	}
	return ""
}

// reportRTCDiagnostic 仅记录当前连接尝试的参与设备提交的计数和阶段。
func (api workspaceAPI) reportRTCDiagnostic(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	id := r.PathValue("transfer_id")
	store, ok := api.store.(rtcDiagnosticStore)
	if !ok || id != configuredRTCDiagnostic() {
		writeError(w, http.StatusNotFound, "diagnostic unavailable")
		return
	}
	var report peer.RTCDiagnostic
	if !readJSONLimit(w, r, &report, 2048) {
		return
	}
	if !report.Valid() {
		writeError(w, http.StatusBadRequest, "invalid RTC diagnostic")
		return
	}
	role, err := store.RTCDiagnosticRole(r.Context(), device, token, id, report.AttemptID)
	if err == nil {
		data, _ := json.Marshal(report)
		log.Printf("rtc_diagnostic transfer=%s role=%s %s", id, role, data)
	}
	workspaceJSON(w, http.StatusNoContent, nil, err)
}

// configuredRTC 为指定设备生成 ICE 配置。登记认证成功后才返回给设备。
// 显式留空 STUN 配置会禁用默认服务器。
func configuredRTC(device string, now time.Time) (peer.RTCConfig, error) {
	config := peer.RTCConfig{ICEServers: []peer.ICEServer{}}
	stun, explicit := os.LookupEnv("MUSTER_RTC_STUN_URLS")
	if !explicit {
		stun = "stun:stun.l.google.com:19302"
	}
	stunURLs, err := iceURLs(stun, "stun", "stuns")
	if err != nil {
		return config, err
	}
	if len(stunURLs) != 0 {
		config.ICEServers = append(config.ICEServers, peer.ICEServer{URLs: stunURLs})
	}
	relay, err := iceURLs(os.Getenv("MUSTER_RTC_TURN_URLS"), "turn", "turns")
	if err != nil {
		return config, err
	}
	server, err := configuredTURN(relay, device, now)
	if err == nil && server.Username != "" {
		config.ICEServers = append(config.ICEServers, server)
	}
	return config, err
}

// configuredTURN 仅给试点名单中的设备签发短期凭据，也支持已有静态配置。
func configuredTURN(relay []string, device string, now time.Time) (peer.ICEServer, error) {
	secret := os.Getenv("MUSTER_RTC_TURN_REST_SECRET")
	devices := strings.TrimSpace(os.Getenv("MUSTER_RTC_TURN_DEVICE_IDS"))
	username := os.Getenv("MUSTER_RTC_TURN_USERNAME")
	credential := os.Getenv("MUSTER_RTC_TURN_CREDENTIAL")
	if len(relay) == 0 && secret == "" && devices == "" {
		return peer.ICEServer{}, nil
	}
	if secret == "" && devices == "" {
		if username == "" || credential == "" || len(username) > 512 || len(credential) > 512 {
			return peer.ICEServer{}, errors.New("TURN requires a bounded username and credential")
		}
		return peer.ICEServer{URLs: relay, Username: username, Credential: credential}, nil
	}
	if len(relay) == 0 || username != "" || credential != "" {
		return peer.ICEServer{}, errors.New("TURN REST requires URLs and cannot be combined with static credentials")
	}
	if len(secret) != 64 || strings.ToLower(secret) != secret {
		return peer.ICEServer{}, errors.New("TURN REST secret requires 32 bytes encoded in lowercase hex")
	}
	if _, err := hex.DecodeString(secret); err != nil {
		return peer.ICEServer{}, errors.New("TURN REST secret requires 32 bytes encoded in lowercase hex")
	}
	allowed := strings.Split(devices, ",")
	for i, id := range allowed {
		allowed[i] = strings.TrimSpace(id)
		if !registry.ValidDeviceID(allowed[i]) {
			return peer.ICEServer{}, errors.New("TURN REST requires a comma-separated list of device IDs")
		}
	}
	if !slices.Contains(allowed, device) {
		return peer.ICEServer{}, nil
	}

	username = strconv.FormatInt(now.Add(turnCredentialLifetime).Unix(), 10) + ":" + device
	// Coturn REST 使用 HMAC-SHA1，输入是完整用户名，密钥是服务端配置的原始字符串。
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(username))
	credential = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return peer.ICEServer{URLs: relay, Username: username, Credential: credential}, nil
}

func iceURLs(value, plain, secure string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	urls := strings.Split(value, ",")
	if len(urls) > 4 {
		return nil, errors.New("ICE server list exceeds four URLs")
	}
	for index, raw := range urls {
		raw = strings.TrimSpace(raw)
		parsed, err := url.Parse(raw)
		if err != nil || len(raw) > 512 || parsed.Opaque == "" || parsed.Fragment != "" ||
			(parsed.Scheme != plain && parsed.Scheme != secure) ||
			strings.ContainsAny(parsed.Opaque, "/@ \\?#") {
			return nil, errors.New("invalid ICE URL")
		}
		if host, _, err := net.SplitHostPort(parsed.Opaque); err == nil && host == "" {
			return nil, errors.New("missing ICE server hostname")
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if parsed.RawQuery != "" && (plain != "turn" || err != nil || len(query) != 1 ||
			len(query["transport"]) != 1 || (query.Get("transport") != "tcp" && query.Get("transport") != "udp")) {
			return nil, errors.New("invalid ICE URL transport")
		}
		urls[index] = raw
	}
	return urls, nil
}

func (api workspaceAPI) rtcOffers(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	store, ok := api.rtcStorage(w)
	if !ok {
		return
	}
	offers, err := store.RTCOffers(r.Context(), device, token)
	workspaceJSON(w, http.StatusOK, peer.RTCOffers{Offers: offers}, err)
}

func (api workspaceAPI) putRTCOffer(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	store, ok := api.rtcStorage(w)
	if !ok {
		return
	}
	id, signal, ok := readRTCSignal(w, r)
	if ok {
		workspaceJSON(w, http.StatusNoContent, nil, store.PutRTCOffer(r.Context(), device, token, id, signal))
	}
}

func (api workspaceAPI) rtcAnswer(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	store, ok := api.rtcStorage(w)
	if !ok {
		return
	}
	id, attempt := r.PathValue("transfer_id"), r.URL.Query().Get("attempt_id")
	if !prefixedUUID(id, "transfer-") || !peer.ValidAttempt(attempt) {
		writeError(w, http.StatusBadRequest, "invalid signaling reference")
		return
	}
	answer, err := store.RTCAnswer(r.Context(), device, token, id, attempt)
	workspaceJSON(w, http.StatusOK, answer, err)
}

func (api workspaceAPI) putRTCAnswer(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	store, ok := api.rtcStorage(w)
	if !ok {
		return
	}
	id, signal, ok := readRTCSignal(w, r)
	if ok {
		workspaceJSON(w, http.StatusNoContent, nil, store.PutRTCAnswer(r.Context(), device, token, id, signal))
	}
}

// readRTCSignal 在写入存储前校验投递、连接尝试与 SDP，限制单条信令的大小。
func readRTCSignal(w http.ResponseWriter, r *http.Request) (string, peer.RTCSignal, bool) {
	id := r.PathValue("transfer_id")
	var signal peer.RTCSignal
	if !readJSONLimit(w, r, &signal, 2*peer.MaxSDPBytes+256) {
		return "", signal, false
	}
	if !prefixedUUID(id, "transfer-") || !peer.ValidAttempt(signal.AttemptID) || !peer.ValidDescription(signal.SDP) {
		writeError(w, http.StatusBadRequest, "invalid device data-channel description")
		return "", signal, false
	}
	return id, signal, true
}
