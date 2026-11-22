// Package httpapi 提供中心服务的 HTTP 接口。
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
)

const (
	maxBodyBytes = 16 << 10
	pairingBytes = 18
	tokenBytes   = 32
)

var bearerPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

type server struct {
	store     registry.Store
	adminHash [32]byte
}

// ValidAdminToken 校验管理员凭据能否用 HTTP Bearer 方式传输。
func ValidAdminToken(token string) bool {
	return len(token) <= 512 && bearerPattern.MatchString(token)
}

// NewHandler 配置健康检查、设备 API 和同源静态 Web 入口。store 为 nil 时仅启用健康检查与静态入口。
func NewHandler(store registry.Store, adminToken string, web http.Handler, options ...WorkspaceOptions) http.Handler {
	s := server{store: store, adminHash: sha256.Sum256([]byte(adminToken))}
	api := http.NewServeMux()
	api.HandleFunc("POST /api/v1/pairing-codes", s.admin(s.createPairing))
	api.HandleFunc("POST /api/v1/devices/enroll", s.enroll)
	api.HandleFunc("GET /api/v1/devices", s.admin(s.list))
	api.HandleFunc("DELETE /api/v1/devices/{device_id}", s.admin(s.revoke))
	api.HandleFunc("POST /api/v1/devices/{device_id}/heartbeat", s.device(s.heartbeat))
	api.HandleFunc("PUT /api/v1/devices/{device_id}/inventory", s.device(s.report))
	s.registerUpdates(api)
	if len(options) != 0 {
		s.registerWorkspace(api, options[0], adminToken)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, struct {
			Status string `json:"status"`
		}{"ok"})
	})
	if web != nil {
		mux.Handle("GET /", web)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" && !strings.HasPrefix(r.URL.Path, "/api/") {
			mux.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		// 避免 ServeMux 通过重定向转发含配对码或令牌的请求。
		if r.URL.Path != path.Clean(r.URL.Path) || r.URL.RawPath != "" {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if s.store == nil {
			writeError(w, http.StatusServiceUnavailable, "device registry unavailable")
			return
		}
		timeout := 5 * time.Second
		if r.URL.Path == "/api/v1/events" {
			timeout = 12 * time.Second
		} else if strings.HasSuffix(r.URL.Path, "/runs/claim") {
			timeout = 8 * time.Second
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		api.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		hash := sha256.Sum256([]byte(token))
		if token == "" || subtle.ConstantTimeCompare(hash[:], s.adminHash[:]) != 1 {
			writeUnauthorized(w)
			return
		}
		next(w, r)
	}
}

func (s server) device(next func(http.ResponseWriter, *http.Request, string, [32]byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		id := r.PathValue("device_id")
		if !registry.ValidDeviceID(id) || !validRandomSecret(token, tokenBytes) {
			writeUnauthorized(w)
			return
		}
		hash := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(hash[:], s.adminHash[:]) == 1 {
			writeUnauthorized(w)
			return
		}
		next(w, r, id, hash)
	}
}

func (s server) createPairing(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if !registry.ValidName(request.Name) {
		writeError(w, http.StatusBadRequest, "invalid device name")
		return
	}

	codeBytes, err := randomBytes(pairingBytes)
	if err != nil {
		storeError(w, err)
		return
	}
	code := base64.RawURLEncoding.EncodeToString(codeBytes)
	expires, err := s.store.CreatePairing(r.Context(), request.Name, sha256.Sum256([]byte(code)))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expires_at"`
	}{code, expires})
}

func (s server) enroll(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Code string `json:"code"`
		registry.Enrollment
	}
	if !readJSON(w, r, &request) {
		return
	}
	if !validRandomSecret(request.Code, pairingBytes) {
		storeError(w, registry.ErrInvalidPairing)
		return
	}
	if err := request.Enrollment.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	idBytes, err := randomBytes(16)
	if err != nil {
		storeError(w, err)
		return
	}
	deviceTokenBytes, err := randomBytes(tokenBytes)
	if err != nil {
		storeError(w, err)
		return
	}
	id := "d-" + hex.EncodeToString(idBytes)
	token := base64.RawURLEncoding.EncodeToString(deviceTokenBytes)
	err = s.store.Enroll(r.Context(), id, request.Enrollment, sha256.Sum256([]byte(request.Code)), sha256.Sum256([]byte(token)))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
	}{id, token})
}

func (s server) heartbeat(w http.ResponseWriter, r *http.Request, id string, tokenHash [32]byte) {
	if r.ContentLength != 0 {
		writeError(w, http.StatusBadRequest, "heartbeat body must be empty")
		return
	}
	if err := s.store.Heartbeat(r.Context(), id, tokenHash); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s server) report(w http.ResponseWriter, r *http.Request, id string, tokenHash [32]byte) {
	var request struct {
		Agents []struct {
			ID           string  `json:"id"`
			Version      *string `json:"version"`
			Status       string  `json:"status"`
			Capabilities struct {
				Prompt     *bool `json:"prompt"`
				FinalReply *bool `json:"final_reply"`
				Resume     *bool `json:"resume"`
			} `json:"capabilities"`
		} `json:"agents"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if request.Agents == nil || len(request.Agents) > 2 {
		writeError(w, http.StatusBadRequest, "invalid agents list")
		return
	}
	inventory := registry.Inventory{Agents: make([]registry.Agent, 0, len(request.Agents))}
	for _, agent := range request.Agents {
		caps := agent.Capabilities
		if agent.Version == nil || caps.Prompt == nil || caps.FinalReply == nil || caps.Resume == nil {
			writeError(w, http.StatusBadRequest, "agent version and capabilities are required")
			return
		}
		inventory.Agents = append(inventory.Agents, registry.Agent{
			ID: agent.ID, Version: *agent.Version, Status: agent.Status,
			Capabilities: &registry.Capabilities{Prompt: *caps.Prompt, FinalReply: *caps.FinalReply, Resume: *caps.Resume},
		})
	}
	if err := inventory.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.Report(r.Context(), id, tokenHash, inventory); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s server) list(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.List(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Devices []registry.Device `json:"devices"`
	}{devices})
}

func (s server) revoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("device_id")
	if !registry.ValidDeviceID(id) {
		storeError(w, registry.ErrNotFound)
		return
	}
	if err := s.store.Revoke(r.Context(), id); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func bearer(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	scheme, token, _ := strings.Cut(values[0], " ")
	token = strings.TrimLeft(token, " ")
	if strings.EqualFold(scheme, "Bearer") && ValidAdminToken(token) {
		return token
	}
	return ""
}

func validRandomSecret(value string, size int) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(size) {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == size
}

func randomBytes(size int) ([]byte, error) {
	buf := make([]byte, size)
	_, err := io.ReadFull(rand.Reader, buf)
	return buf, err
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	return readJSONLimit(w, r, target, maxBodyBytes)
}

func readWorkspaceText(w http.ResponseWriter, r *http.Request, target any) bool {
	return readJSONLimit(w, r, target, 80<<10)
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var sizeError *http.MaxBytesError
	if errors.As(err, &sizeError) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	if err != nil || !utf8.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized")
}

func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrInvalidPairing):
		writeError(w, http.StatusBadRequest, "invalid pairing code")
	case errors.Is(err, registry.ErrUnauthorized):
		writeUnauthorized(w)
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "device not found")
	default:
		log.Printf("device registry operation failed (%T)", err)
		writeError(w, http.StatusServiceUnavailable, "device registry unavailable")
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
