package httpapi

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

const (
	runLogTimeout = 12 * time.Second
	maxRunLogs    = 32
)

// runLogAPI 仅在管理员 HTTP 请求存续时保存日志请求和设备回执。
type runLogAPI struct {
	store    workspace.Store
	mu       sync.Mutex
	requests map[string]*pendingRunLog
}

type pendingRunLog struct {
	request   runreport.LogRequest
	ctx       context.Context
	device    string
	reply     chan runreport.LogResponse
	uploading bool
	complete  bool
}

func (api *runLogAPI) read(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "run logs require GET")
		return
	}
	taskID, runID := r.PathValue("id"), r.PathValue("run_id")
	if !prefixedUUID(taskID, "task-") || !prefixedUUID(runID, "run-") {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	device, err := api.store.RunLogDevice(r.Context(), taskID, runID)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "run not found")
		return
	case errors.Is(err, workspace.ErrRunLogsUnavailable), errors.Is(err, workspace.ErrRunLogDeviceRevoked):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, workspace.ErrRunLogDeviceOffline):
		writeError(w, http.StatusGatewayTimeout, err.Error())
		return
	case err != nil:
		storeError(w, err)
		return
	}
	id, err := randomBytes(16)
	if err != nil {
		storeError(w, err)
		return
	}
	pending := &pendingRunLog{
		request: runreport.LogRequest{ID: "log-" + hex.EncodeToString(id), RunID: runID},
		ctx:     r.Context(), device: device, reply: make(chan runreport.LogResponse, 1),
	}
	if !api.enqueue(pending) {
		writeError(w, http.StatusTooManyRequests, "too many pending run log requests")
		return
	}
	defer func() {
		api.mu.Lock()
		delete(api.requests, pending.request.ID)
		api.mu.Unlock()
	}()
	var response runreport.LogResponse
	select {
	case response = <-pending.reply:
	case <-r.Context().Done():
	}
	switch err := r.Context().Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "execution device did not respond to the log request")
	case err == nil:
		writeJSON(w, http.StatusOK, response)
	}
}

func (api *runLogAPI) enqueue(pending *pendingRunLog) bool {
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.requests) >= maxRunLogs {
		return false
	}
	count := 0
	for _, active := range api.requests {
		if active.device == pending.device {
			count++
		}
	}
	if count >= runreport.MaxLogRequests {
		return false
	}
	api.requests[pending.request.ID] = pending
	return true
}

func (api *runLogAPI) list(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	if err := api.store.AuthenticateRunLogDevice(r.Context(), device, token); err != nil {
		storeError(w, err)
		return
	}
	requests := make([]runreport.LogRequest, 0, runreport.MaxLogRequests)
	api.mu.Lock()
	for _, pending := range api.requests {
		if pending.device == device && !pending.complete && !pending.uploading && pending.ctx.Err() == nil {
			requests = append(requests, pending.request)
		}
	}
	api.mu.Unlock()
	writeJSON(w, http.StatusOK, struct {
		Requests []runreport.LogRequest `json:"requests"`
	}{requests})
}

func (api *runLogAPI) submit(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	if err := api.store.AuthenticateRunLogDevice(r.Context(), device, token); err != nil {
		storeError(w, err)
		return
	}
	pending, status := api.reserveUpload(r.PathValue("request_id"), device)
	if status != http.StatusNoContent {
		writeError(w, status, "run log request is unavailable")
		return
	}
	defer func() {
		api.mu.Lock()
		pending.uploading = false
		api.mu.Unlock()
	}()
	var response runreport.LogResponse
	if !readJSONLimit(w, r, &response, runreport.MaxLogResponseBytes) {
		return
	}
	if err := response.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 上传可能持续数秒。完整读取后重新鉴权，避免转发读取期间撤销的设备回执。
	if err := api.store.AuthenticateRunLogDevice(r.Context(), device, token); err != nil {
		storeError(w, err)
		return
	}
	api.mu.Lock()
	deliver := pending.ctx.Err() == nil
	if deliver {
		pending.complete = true
		pending.reply <- response
	}
	api.mu.Unlock()
	if !deliver {
		writeError(w, http.StatusNotFound, "run log request is unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reserveUpload 限制同一请求同时只解码一份正文。过期及其他设备的请求均返回 404。
func (api *runLogAPI) reserveUpload(id, device string) (*pendingRunLog, int) {
	api.mu.Lock()
	defer api.mu.Unlock()
	pending := api.requests[id]
	switch {
	case pending == nil || pending.device != device || pending.complete || pending.ctx.Err() != nil:
		return nil, http.StatusNotFound
	case pending.uploading:
		return nil, http.StatusTooManyRequests
	default:
		pending.uploading = true
		return pending, http.StatusNoContent
	}
}
