package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/artifact"
	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/baiyucraft/agent-muster/internal/inputreceipt"
)

// WorkspaceOptions 配置工作区、中心插件令牌和管理员会话。
// 令牌须通过格式校验，并与管理员令牌不同，工作区接口才能启用。
type WorkspaceOptions struct {
	Store       workspace.Store
	PluginToken string
	AdminAuth   AdminAuthOptions
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
	logs := runLogAPI{store: options.Store, requests: make(map[string]*pendingRunLog)}
	artifacts := artifactAPI{store: options.Store}
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
	admin("GET /api/v1/workspace/usage", api.deviceUsage)
	admin("GET /api/v1/tasks/{id}", api.task)
	admin("GET /api/v1/tasks/{id}/runs/{run_id}/logs", logs.read)
	admin("GET /api/v1/runs/{id}/artifacts/{artifact_id}", artifacts.read)
	admin("POST /api/v1/tasks/{id}/control", api.controlTask)
	admin("PUT /api/v1/tasks/{id}/takeover", api.takeOverTask)
	admin("GET /api/v1/events", api.events)
	admin("POST /api/v1/approvals/{id}/answer", api.answerApproval)
	admin("PUT /api/v1/projects/{id}", api.saveProject)
	admin("PUT /api/v1/binding-proposals/{id}/decision", api.decideBinding)
	admin("POST /api/v1/conversation/messages", api.submitMessage)
	plugin("GET /api/v1/plugin/projects", api.projects)
	plugin("GET /api/v1/plugin/binding-candidates", api.bindingCandidates)
	plugin("GET /api/v1/plugin/tasks/{id}", api.task)
	plugin("POST /api/v1/plugin/deliveries/claim", api.claimDelivery)
	plugin("PUT /api/v1/plugin/deliveries/{id}/lease", api.renew)
	plugin("PUT /api/v1/plugin/deliveries/{id}/complete", api.complete)
	plugin("POST /api/v1/plugin/deliveries/{id}/dispatch", api.dispatch)
	plugin("POST /api/v1/plugin/deliveries/{id}/binding-proposal", api.proposeBinding)
	device("PUT /api/v1/devices/{device_id}/directories", api.directories)
	device("GET /api/v1/devices/{device_id}/run-log-requests", logs.list)
	device("PUT /api/v1/devices/{device_id}/run-log-requests/{request_id}", logs.submit)
	device("GET /api/v1/devices/{device_id}/artifact-reads", artifacts.list)
	device("PUT /api/v1/devices/{device_id}/artifact-reads/{request_id}", artifacts.submit)
	device("PUT /api/v1/devices/{device_id}/peer", api.registerPeer)
	device("PUT /api/v1/devices/{device_id}/rtc", api.registerRTC)
	device("PUT /api/v1/devices/{device_id}/rtc/public-key", api.reportRTCPublicKey)
	device("GET /api/v1/devices/{device_id}/rtc/offers", api.rtcOffers)
	device("PUT /api/v1/devices/{device_id}/rtc/{transfer_id}/offer", api.putRTCOffer)
	device("GET /api/v1/devices/{device_id}/rtc/{transfer_id}/answer", api.rtcAnswer)
	device("PUT /api/v1/devices/{device_id}/rtc/{transfer_id}/answer", api.putRTCAnswer)
	device("PUT /api/v1/devices/{device_id}/rtc/{transfer_id}/diagnostic", api.reportRTCDiagnostic)
	device("POST /api/v1/devices/{device_id}/runs/claim", api.claimRun)
	device("POST /api/v1/devices/{device_id}/runs/{id}/control", api.runControl)
	device("PUT /api/v1/devices/{device_id}/runs/{id}/lease", api.renewRun)
	device("POST /api/v1/devices/{device_id}/runs/{id}/wait/validate", api.validateWait)
	device("POST /api/v1/devices/{device_id}/runs/{id}/git-push", api.authorizeGitPush)
	device("PUT /api/v1/devices/{device_id}/runs/{id}/git-push/{intent_id}/observation", api.observeGitPush)
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

func (api workspaceAPI) deviceUsage(w http.ResponseWriter, r *http.Request) {
	value, err := api.store.DeviceUsage(r.Context())
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
	var request struct {
		workspace.Project
		GitPolicy json.RawMessage `json:"git_policy"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	input := request.Project
	if len(request.GitPolicy) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(request.GitPolicy))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input.GitPolicy); err != nil {
			writeError(w, http.StatusBadRequest, "invalid Git delivery policy")
			return
		}
		input.ClearGitPolicy = input.GitPolicy == nil
		if input.GitPolicy != nil {
			input.GitPolicy.Revision = 0
			if err := input.GitPolicy.Validate(); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
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
	capabilities := workspace.RunClaimCapabilities{
		Control:            r.Header.Get("Muster-Run-Control") == "1",
		GitVerification:    r.Header.Get("Muster-Git-Verification") == "1",
		GitPublish:         r.Header.Get("Muster-Git-Publish") == "1",
		InputReceipt:       r.Header.Get(inputreceipt.Header) == "1",
		GitCandidateCommit: r.Header.Get(gitdelivery.CandidateCommitHeader) == "1",
	}
	for {
		run, err := api.store.ClaimCompatibleRun(ctx, device, token, capabilities)
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
	if !readJSONLimit(w, r, &event, 256<<10) {
		return
	}
	if !artifact.ValidFor(id, event.Artifacts) {
		writeError(w, http.StatusBadRequest, "invalid run artifacts")
		return
	}
	if event.SessionFailure != "" && (event.Kind != "failed" || event.SessionID != "" ||
		(event.SessionFailure != "busy" && event.SessionFailure != "unavailable") || !validRunLease(event.Generation, event.Lease)) {
		writeError(w, http.StatusBadRequest, "invalid session failure")
		return
	}
	if !validText(event.Text, 12000, false) || (event.SessionID != "" && !sessionPattern.MatchString(event.SessionID)) ||
		!validEventLease(event.Generation, event.Lease) || !validTransfers(event.TransferIDs) ||
		!validIDs(event.WaitForTaskIDs, "task-") || (event.Kind != "waiting" && len(event.WaitForTaskIDs) != 0) ||
		!event.InputReceipt.Valid() || (event.InputReceipt != nil &&
		(event.Kind == "started" || event.Kind == "session" || event.Kind == "stopped")) {
		writeError(w, http.StatusBadRequest, "invalid run event data")
		return
	}
	switch event.Kind {
	case "started", "interrupted", "failed":
	case "stopped":
		if !validRunLease(event.Generation, event.Lease) || len(event.TransferIDs) != 0 || event.Approval != nil {
			writeError(w, http.StatusBadRequest, "invalid stopped run event")
			return
		}
	case "approval_requested":
		if event.Approval == nil || len(event.TransferIDs) != 0 {
			writeError(w, http.StatusBadRequest, "approval requires a question and cannot include transfers")
			return
		}
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

func (api workspaceAPI) answerApproval(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !prefixedUUID(id, "approval-") {
		writeError(w, http.StatusBadRequest, "invalid approval id")
		return
	}
	var input struct {
		Answer string `json:"answer"`
	}
	if !readWorkspaceText(w, r, &input) {
		return
	}
	value, err := api.store.AnswerApproval(r.Context(), id, input.Answer)
	workspaceJSON(w, http.StatusOK, value, err)
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
	value.UsageSupported = true
	value.PlanSupported = true
	value.BindingProposalsSupported = true
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
		(input.Status != "handled" && input.Status != "interrupted") || !validText(input.Text, 12000, false) ||
		!input.Usage.ValidCenterUsage() || (input.Usage != nil && input.Status != "handled") {
		writeError(w, http.StatusBadRequest, "invalid delivery result")
		return
	}
	workspaceJSON(w, http.StatusNoContent, nil, api.store.CompleteDelivery(r.Context(), id, input))
}

func (api workspaceAPI) dispatch(w http.ResponseWriter, r *http.Request) {
	var input workspace.Dispatch
	// JSON 转义可能把一个字节扩为六字节。为八项计划保留完整提示词及结构空间。
	if !readJSONLimit(w, r, &input, 320<<10) {
		return
	}
	id := r.PathValue("id")
	create := input.Steps == nil && input.TaskID == "" && (!input.GitPublish || input.GitDelivery) && uuidPattern.MatchString(input.ProjectID) && validText(input.Title, 240, true) &&
		(input.BindingID == "" || uuidPattern.MatchString(input.BindingID))
	appendToTask := input.Steps == nil && !input.GitDelivery && !input.GitPublish && input.ProjectID == "" && input.BindingID == "" && prefixedUUID(input.TaskID, "task-")
	plan := input.TaskID == "" && input.ProjectID == "" && input.BindingID == "" && input.Prompt == "" &&
		!input.GitDelivery && !input.GitPublish && validText(input.Title, 240, true) && validDispatchSteps(input.Steps)
	if !prefixedUUID(id, "message-") || !leasePattern.MatchString(input.Lease) ||
		(!plan && (!(create || appendToTask) || !validText(input.Prompt, 6000, true))) {
		writeError(w, http.StatusBadRequest, "invalid task dispatch")
		return
	}
	taskID, err := api.store.Dispatch(r.Context(), id, input)
	workspaceJSON(w, http.StatusOK, struct {
		TaskID string `json:"task_id"`
	}{taskID}, err)
}

func validDispatchSteps(steps []workspace.DispatchStep) bool {
	if len(steps) < 2 || len(steps) > 8 {
		return false
	}
	for index, step := range steps {
		if !uuidPattern.MatchString(step.ProjectID) || step.BindingID != "" && !uuidPattern.MatchString(step.BindingID) ||
			!validText(step.Title, 240, true) || !validText(step.Prompt, 6000, true) {
			return false
		}
		seen := make(map[int]bool, len(step.DependsOn))
		for _, prerequisite := range step.DependsOn {
			if prerequisite < 1 || prerequisite > index || seen[prerequisite] {
				return false
			}
			seen[prerequisite] = true
		}
	}
	return true
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
	case errors.Is(err, workspace.ErrInvalidApproval):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workspace.ErrInvalidRunDiagnostics):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workspace.ErrInvalidGitVerification):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workspace.ErrInvalidGitPublication):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workspace.ErrInvalidInputReceipt):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workspace.ErrInvalidSessionFailure):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workspace.ErrInvalidArtifacts):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workspace.ErrInvalidBindingProposal):
		writeError(w, http.StatusBadRequest, err.Error())
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
