package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

// WorkspaceOptions 配置工作区存储和中心插件令牌。
// 令牌须通过格式校验，并与管理员令牌不同，工作区接口才能启用。
type WorkspaceOptions struct {
	Store       workspace.Store
	PluginToken string
}

type workspaceAPI struct {
	store workspace.Store
	token [32]byte
}

var (
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	directoryPattern = regexp.MustCompile(`^dir-[a-f0-9]{32}$`)
	sessionPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)
	leasePattern     = regexp.MustCompile(`^[A-Z2-7]{26}$`)
)

func (s server) registerWorkspace(mux *http.ServeMux, options WorkspaceOptions, adminToken string) {
	api := workspaceAPI{store: options.Store, token: sha256.Sum256([]byte(options.PluginToken))}
	enabled := options.Store != nil && ValidAdminToken(options.PluginToken) && options.PluginToken != adminToken
	available := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !enabled {
				writeError(w, http.StatusServiceUnavailable, "workspace unavailable")
				return
			}
			next(w, r)
		}
	}
	admin := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, available(s.admin(handler)))
	}
	plugin := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, available(api.plugin(handler)))
	}
	device := func(pattern string, handler func(http.ResponseWriter, *http.Request, string, [32]byte)) {
		mux.HandleFunc(pattern, available(s.device(handler)))
	}
	admin("GET /api/v1/workspace", api.snapshot)
	admin("GET /api/v1/tasks/{id}", api.task)
	admin("GET /api/v1/events", api.events)
	admin("PUT /api/v1/projects/{id}", api.saveProject)
	admin("POST /api/v1/conversation/messages", api.submitMessage)
	plugin("GET /api/v1/plugin/projects", api.projects)
	plugin("GET /api/v1/plugin/tasks/{id}", api.task)
	plugin("POST /api/v1/plugin/deliveries/claim", api.claimDelivery)
	plugin("PUT /api/v1/plugin/deliveries/{id}/lease", api.renew)
	plugin("PUT /api/v1/plugin/deliveries/{id}/complete", api.complete)
	plugin("POST /api/v1/plugin/deliveries/{id}/dispatch", api.dispatch)
	device("PUT /api/v1/devices/{device_id}/directories", api.directories)
	device("PUT /api/v1/devices/{device_id}/peer", api.registerPeer)
	device("PUT /api/v1/devices/{device_id}/rtc", api.registerRTC)
	device("PUT /api/v1/devices/{device_id}/rtc/public-key", api.reportRTCPublicKey)
	device("GET /api/v1/devices/{device_id}/rtc/offers", api.rtcOffers)
	device("PUT /api/v1/devices/{device_id}/rtc/{transfer_id}/offer", api.putRTCOffer)
	device("GET /api/v1/devices/{device_id}/rtc/{transfer_id}/answer", api.rtcAnswer)
	device("PUT /api/v1/devices/{device_id}/rtc/{transfer_id}/answer", api.putRTCAnswer)
	device("PUT /api/v1/devices/{device_id}/rtc/{transfer_id}/diagnostic", api.reportRTCDiagnostic)
	device("POST /api/v1/devices/{device_id}/runs/claim", api.claimRun)
	device("PUT /api/v1/devices/{device_id}/runs/{id}/lease", api.renewRun)
	device("POST /api/v1/devices/{device_id}/runs/{id}/wait/validate", api.validateWait)
	device("PUT /api/v1/devices/{device_id}/runs/{id}/events/{sequence}", api.recordEvent)
	device("POST /api/v1/devices/{device_id}/runs/{id}/children", api.delegate)
	device("POST /api/v1/devices/{device_id}/runs/{id}/transfers", api.createTransfer)
	device("GET /api/v1/devices/{device_id}/transfers/outgoing", api.outgoingTransfers)
	device("POST /api/v1/devices/{device_id}/transfers/{transfer_id}/authorize", api.authorizeTransfer)
	device("PUT /api/v1/devices/{device_id}/transfers/{transfer_id}/error", api.reportTransferError)
	device("POST /api/v1/devices/{device_id}/transfers/{transfer_id}/verify", api.verifyTransfer)
	device("PUT /api/v1/devices/{device_id}/transfers/{transfer_id}/ack", api.acknowledgeTransfer)
}

func (api workspaceAPI) plugin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		hash := sha256.Sum256([]byte(token))
		if token == "" || subtle.ConstantTimeCompare(hash[:], api.token[:]) != 1 {
			writeUnauthorized(w)
			return
		}
		next(w, r)
	}
}

func (api workspaceAPI) snapshot(w http.ResponseWriter, r *http.Request) {
	value, err := api.store.Workspace(r.Context())
	workspaceJSON(w, http.StatusOK, value, err)
}

func (api workspaceAPI) task(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !prefixedUUID(id, "task-") {
		storeError(w, registry.ErrNotFound)
		return
	}
	value, err := api.store.TaskDetail(r.Context(), id)
	workspaceJSON(w, http.StatusOK, value, err)
}

func (api workspaceAPI) saveProject(w http.ResponseWriter, r *http.Request) {
	var input workspace.Project
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	input.Name = strings.TrimSpace(input.Name)
	if !uuidPattern.MatchString(id) || !validText(input.Name, 120, true) || !validProjectBindings(input) {
		writeError(w, http.StatusBadRequest, "invalid project or directory binding")
		return
	}
	input.ID = id
	value, err := api.store.SaveProject(r.Context(), input)
	workspaceJSON(w, http.StatusOK, value, err)
}

func (api workspaceAPI) submitMessage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID     string `json:"id"`
		Text   string `json:"text"`
		TaskID string `json:"task_id"`
	}
	if !readWorkspaceText(w, r, &input) {
		return
	}
	if !uuidPattern.MatchString(input.ID) || !validText(input.Text, 6000, true) ||
		(input.TaskID != "" && !prefixedUUID(input.TaskID, "task-")) {
		writeError(w, http.StatusBadRequest, "invalid message")
		return
	}
	value, err := api.store.SubmitMessage(r.Context(), workspace.Message{ID: input.ID, Text: input.Text, TaskID: input.TaskID})
	workspaceJSON(w, http.StatusAccepted, value, err)
}

func (api workspaceAPI) directories(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	var input struct {
		Directories []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"directories"`
	}
	if !readWorkspaceText(w, r, &input) {
		return
	}
	if input.Directories == nil || len(input.Directories) > 32 {
		writeError(w, http.StatusBadRequest, "invalid directory list")
		return
	}
	seen := make(map[string]bool, len(input.Directories))
	dirs := make([]workspace.Directory, 0, len(input.Directories))
	for _, d := range input.Directories {
		if !directoryPattern.MatchString(d.ID) || seen[d.ID] || !validText(d.Label, 120, true) {
			writeError(w, http.StatusBadRequest, "invalid directory")
			return
		}
		seen[d.ID] = true
		dirs = append(dirs, workspace.Directory{ID: d.ID, Label: d.Label})
	}
	workspaceJSON(w, http.StatusNoContent, nil, api.store.ReplaceDirectories(r.Context(), device, token, dirs))
}

func (api workspaceAPI) claimRun(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	if r.ContentLength != 0 {
		writeError(w, http.StatusBadRequest, "claim body must be empty")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		run, err := api.store.ClaimRun(ctx, device, token)
		if ctx.Err() != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil || run.ID != "" {
			workspaceJSON(w, http.StatusOK, run, err)
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
}

func (api workspaceAPI) recordEvent(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	id := r.PathValue("id")
	sequence, err := strconv.ParseInt(r.PathValue("sequence"), 10, 64)
	if err != nil || sequence < 1 || !prefixedUUID(id, "run-") {
		writeError(w, http.StatusBadRequest, "invalid run event identity")
		return
	}
	var event workspace.RunEvent
	if !readWorkspaceText(w, r, &event) {
		return
	}
	if !validText(event.Text, 12000, false) || (event.SessionID != "" && !sessionPattern.MatchString(event.SessionID)) ||
		!validEventLease(event.Generation, event.Lease) || !validTransfers(event.TransferIDs) ||
		!validIDs(event.WaitForTaskIDs, "task-") || (event.Kind != "waiting" && len(event.WaitForTaskIDs) != 0) {
		writeError(w, http.StatusBadRequest, "invalid run event data")
		return
	}
	switch event.Kind {
	case "started", "interrupted", "failed":
	case "waiting":
		if !validWait(event) {
			writeError(w, http.StatusBadRequest, "wait requires dependencies")
			return
		}
	case "session":
		if event.SessionID == "" {
			writeError(w, http.StatusBadRequest, "session id required")
			return
		}
	case "completed":
		if strings.TrimSpace(event.Text) == "" && len(event.TransferIDs) == 0 {
			writeError(w, http.StatusBadRequest, "final reply required")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "unknown run event")
		return
	}
	workspaceJSON(w, http.StatusNoContent, nil, api.store.RecordRunEvent(r.Context(), device, token, id, sequence, event))
}

func (api workspaceAPI) projects(w http.ResponseWriter, r *http.Request) {
	values, err := api.store.Projects(r.Context())
	workspaceJSON(w, http.StatusOK, struct {
		Projects []workspace.Project `json:"projects"`
	}{values}, err)
}

func (api workspaceAPI) claimDelivery(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 {
		writeError(w, http.StatusBadRequest, "claim body must be empty")
		return
	}
	value, err := api.store.ClaimDelivery(r.Context())
	if err == nil && value.ID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	workspaceJSON(w, http.StatusOK, value, err)
}

func (api workspaceAPI) renew(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Lease string `json:"lease"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !deliveryID(id) || !leasePattern.MatchString(input.Lease) {
		writeError(w, http.StatusBadRequest, "invalid delivery lease")
		return
	}
	workspaceJSON(w, http.StatusNoContent, nil, api.store.RenewDelivery(r.Context(), id, input.Lease))
}

func (api workspaceAPI) complete(w http.ResponseWriter, r *http.Request) {
	var input workspace.Completion
	if !readWorkspaceText(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !deliveryID(id) || !leasePattern.MatchString(input.Lease) ||
		(input.Status != "handled" && input.Status != "interrupted") || !validText(input.Text, 12000, false) {
		writeError(w, http.StatusBadRequest, "invalid delivery result")
		return
	}
	workspaceJSON(w, http.StatusNoContent, nil, api.store.CompleteDelivery(r.Context(), id, input))
}

func (api workspaceAPI) dispatch(w http.ResponseWriter, r *http.Request) {
	var input workspace.Dispatch
	if !readWorkspaceText(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	create := input.TaskID == "" && uuidPattern.MatchString(input.ProjectID) && validText(input.Title, 240, true) &&
		(input.BindingID == "" || uuidPattern.MatchString(input.BindingID))
	appendToTask := input.ProjectID == "" && input.BindingID == "" && prefixedUUID(input.TaskID, "task-")
	if !prefixedUUID(id, "message-") || !leasePattern.MatchString(input.Lease) ||
		(!create && !appendToTask) || !validText(input.Prompt, 6000, true) {
		writeError(w, http.StatusBadRequest, "invalid task dispatch")
		return
	}
	taskID, err := api.store.Dispatch(r.Context(), id, input)
	workspaceJSON(w, http.StatusOK, struct {
		TaskID string `json:"task_id"`
	}{taskID}, err)
}

func (api workspaceAPI) events(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		writeError(w, http.StatusBadRequest, "invalid workspace cursor")
		return
	}
	events, err := api.store.WorkspaceEvents(r.Context(), after)
	if errors.Is(err, workspace.ErrConflict) {
		writeError(w, http.StatusConflict, "refresh workspace snapshot")
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	if _, err := fmt.Fprint(w, ": workspace\n\n"); err != nil || controller.Flush() != nil {
		return
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, revision := range events {
			if _, err := fmt.Fprintf(w, "id: %d\nevent: workspace\ndata: {}\n\n", revision); err != nil || controller.Flush() != nil {
				return
			}
			after = revision
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		events, err = api.store.WorkspaceEvents(r.Context(), after)
		if err != nil {
			return
		}
	}
}

func workspaceJSON(w http.ResponseWriter, status int, value any, err error) {
	switch {
	case errors.Is(err, workspace.ErrConflict):
		writeError(w, http.StatusConflict, "workspace state changed or request conflicts with a saved result")
	case err != nil:
		storeError(w, err)
	case status == http.StatusNoContent:
		w.WriteHeader(status)
	default:
		writeJSON(w, status, value)
	}
}

func prefixedUUID(id, prefix string) bool {
	value, ok := strings.CutPrefix(id, prefix)
	return ok && uuidPattern.MatchString(value)
}

func deliveryID(id string) bool {
	return prefixedUUID(id, "message-") || prefixedUUID(id, "report-run-")
}

func validText(value string, limit int, required bool) bool {
	return len(value) <= limit && !strings.ContainsRune(value, 0) && (!required || strings.TrimSpace(value) != "")
}
