package httpapi

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/peer"
)

// rtcStore 按设备身份和投递双方的关系校验信令的读写权限。
type rtcStore interface {
	RegisterRTC(context.Context, string, [32]byte) error
	RTCOffers(context.Context, string, [32]byte) ([]peer.RTCOffer, error)
	PutRTCOffer(context.Context, string, [32]byte, string, peer.RTCSignal) error
	RTCAnswer(context.Context, string, [32]byte, string, string) (peer.RTCAnswer, error)
	PutRTCAnswer(context.Context, string, [32]byte, string, peer.RTCSignal) error
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
	config, err := configuredRTC()
	if err != nil {
		log.Printf("device ICE configuration invalid: %v", err)
		writeError(w, http.StatusServiceUnavailable, "invalid device ICE configuration")
		return
	}
	workspaceJSON(w, http.StatusOK, config, store.RegisterRTC(r.Context(), device, token))
}

// configuredRTC 生成设备注册响应中的 ICE 配置。显式留空 STUN 配置会禁用默认服务器。
func configuredRTC() (peer.RTCConfig, error) {
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
	if len(relay) != 0 {
		username := os.Getenv("MUSTER_RTC_TURN_USERNAME")
		credential := os.Getenv("MUSTER_RTC_TURN_CREDENTIAL")
		if username == "" || credential == "" || len(username) > 512 || len(credential) > 512 {
			return config, errors.New("TURN requires a bounded username and credential")
		}
		config.ICEServers = append(config.ICEServers, peer.ICEServer{URLs: relay, Username: username, Credential: credential})
	}
	return config, nil
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
