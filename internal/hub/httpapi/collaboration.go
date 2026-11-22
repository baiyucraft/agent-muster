package httpapi

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

var (
	hashPattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	requestKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	tailscaleIPv4     = mustCIDR("100.64.0.0/10")
	tailscaleIPv6     = mustCIDR("fd7a:115c:a1e0::/48")
)

func (api workspaceAPI) renewRun(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	var input struct {
		Generation int64  `json:"generation"`
		Lease      string `json:"lease"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !prefixedUUID(id, "run-") || !validRunLease(input.Generation, input.Lease) {
		writeError(w, http.StatusBadRequest, "invalid run lease")
		return
	}
	until, err := api.store.RenewRun(r.Context(), device, token, id, input.Generation, input.Lease)
	workspaceJSON(w, http.StatusOK, struct {
		LeaseUntil time.Time `json:"lease_until"`
	}{until}, err)
}

func (api workspaceAPI) validateWait(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	var input workspace.RunEvent
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !prefixedUUID(id, "run-") || (input.Kind != "" && input.Kind != "waiting") ||
		!validWait(input) || input.SessionID != "" ||
		!validIDs(input.WaitForTaskIDs, "task-") || !validTransfers(input.TransferIDs) {
		writeError(w, http.StatusBadRequest, "invalid wait request")
		return
	}
	workspaceJSON(w, http.StatusNoContent, nil, api.store.ValidateWait(r.Context(), device, token, id, input))
}

func (api workspaceAPI) delegate(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	var input workspace.Delegation
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !prefixedUUID(id, "run-") || !validRunLease(input.Generation, input.Lease) ||
		!requestKeyPattern.MatchString(input.RequestKey) || (input.TaskID != "" && !prefixedUUID(input.TaskID, "task-")) ||
		!uuidPattern.MatchString(input.ProjectID) || !uuidPattern.MatchString(input.BindingID) ||
		!validText(input.Title, 240, true) || !prefixedUUID(input.PromptTransferID, "transfer-") || !validTransfers(input.TransferIDs) {
		writeError(w, http.StatusBadRequest, "invalid child task request")
		return
	}
	task, err := api.store.Delegate(r.Context(), device, token, id, input)
	workspaceJSON(w, http.StatusAccepted, task, err)
}

func (api workspaceAPI) registerPeer(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	var input struct {
		Address     string `json:"address"`
		Fingerprint string `json:"fingerprint"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !validPeerAddress(input.Address) || !hashPattern.MatchString(input.Fingerprint) {
		writeError(w, http.StatusBadRequest, "invalid peer address or certificate fingerprint")
		return
	}
	peer := workspace.Peer{
		DeviceID: device, Address: strings.TrimSuffix(input.Address, "/"), Fingerprint: input.Fingerprint,
	}
	workspaceJSON(w, http.StatusNoContent, nil, api.store.RegisterPeer(r.Context(), device, token, peer))
}

func (api workspaceAPI) createTransfer(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	var input workspace.TransferRequest
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !prefixedUUID(id, "run-") || !validRunLease(input.Generation, input.Lease) ||
		!prefixedUUID(input.ID, "transfer-") || !registry.ValidDeviceID(input.TargetDeviceID) ||
		!validText(input.Name, 255, true) || strings.ContainsAny(input.Name, `:/\`) ||
		input.Name == "." || input.Name == ".." || !validTransferKind(input.Kind) ||
		!hashPattern.MatchString(input.SHA256) || input.Size < 0 || input.Size > 32<<20 ||
		(input.Kind == "message" && input.Size > 64<<10) {
		writeError(w, http.StatusBadRequest, "invalid transfer manifest")
		return
	}
	value, err := api.store.CreateTransfer(r.Context(), device, token, id, input)
	workspaceJSON(w, http.StatusCreated, value, err)
}

func (api workspaceAPI) outgoingTransfers(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	if r.ContentLength != 0 {
		writeError(w, http.StatusBadRequest, "request body must be empty")
		return
	}
	values, err := api.store.OutgoingTransfers(r.Context(), device, token)
	if values == nil {
		values = []workspace.Transfer{}
	}
	workspaceJSON(w, http.StatusOK, struct {
		Transfers []workspace.Transfer `json:"transfers"`
	}{values}, err)
}

func (api workspaceAPI) authorizeTransfer(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	id := r.PathValue("transfer_id")
	if r.ContentLength != 0 || !prefixedUUID(id, "transfer-") {
		writeError(w, http.StatusBadRequest, "invalid transfer identity or request body")
		return
	}
	value, err := api.store.AuthorizeTransfer(r.Context(), device, token, id)
	workspaceJSON(w, http.StatusOK, value, err)
}

func (api workspaceAPI) reportTransferError(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	var input struct {
		Reason string `json:"reason"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("transfer_id")
	if !prefixedUUID(id, "transfer-") {
		writeError(w, http.StatusBadRequest, "invalid transfer identity")
		return
	}
	switch input.Reason {
	case "peer_unavailable", "verification_failed", "delivery_failed":
	default:
		writeError(w, http.StatusBadRequest, "unknown transfer error")
		return
	}
	workspaceJSON(w, http.StatusNoContent, nil, api.store.ReportTransferError(r.Context(), device, token, id, input.Reason))
}

func (api workspaceAPI) verifyTransfer(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	id, ticket, ok := readTransferTicket(w, r)
	if !ok {
		return
	}
	value, err := api.store.VerifyTransfer(r.Context(), device, token, id, ticket)
	workspaceJSON(w, http.StatusOK, value, err)
}

func (api workspaceAPI) acknowledgeTransfer(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	id, ticket, ok := readTransferTicket(w, r)
	if !ok {
		return
	}
	workspaceJSON(w, http.StatusNoContent, nil, api.store.AcknowledgeTransfer(r.Context(), device, token, id, ticket))
}

func readTransferTicket(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	var input struct {
		Ticket string `json:"ticket"`
	}
	if !readJSON(w, r, &input) {
		return "", "", false
	}
	id := r.PathValue("transfer_id")
	if !prefixedUUID(id, "transfer-") || !hashPattern.MatchString(input.Ticket) {
		writeError(w, http.StatusBadRequest, "invalid transfer authorization")
		return "", "", false
	}
	return id, input.Ticket, true
}

func validRunLease(generation int64, lease string) bool {
	return generation > 0 && leasePattern.MatchString(lease)
}

func validEventLease(generation int64, lease string) bool {
	return (generation == 0 && lease == "") || validRunLease(generation, lease)
}

func validWait(event workspace.RunEvent) bool {
	return validRunLease(event.Generation, event.Lease) && event.Text == "" &&
		len(event.WaitForTaskIDs)+len(event.TransferIDs) > 0
}

func validTransferKind(kind string) bool {
	switch kind {
	case "prompt", "file", "message", "result":
		return true
	}
	return false
}

func validTransfers(values []string) bool {
	return validIDs(values, "transfer-")
}

func validIDs(values []string, prefix string) bool {
	if len(values) > 32 {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !prefixedUUID(value, prefix) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func validProjectBindings(project workspace.Project) bool {
	if len(project.Bindings) == 0 {
		return registry.ValidDeviceID(project.DeviceID) && directoryPattern.MatchString(project.DirectoryID) &&
			(project.AgentKind == "" || validAgentKind(project.AgentKind))
	}
	if len(project.Bindings) > 32 {
		return false
	}
	ids := make(map[string]bool, len(project.Bindings))
	defaults := 0
	for _, binding := range project.Bindings {
		if !uuidPattern.MatchString(binding.ID) || ids[binding.ID] || !registry.ValidDeviceID(binding.DeviceID) ||
			!directoryPattern.MatchString(binding.DirectoryID) || !validAgentKind(binding.AgentKind) {
			return false
		}
		ids[binding.ID] = true
		if binding.IsDefault {
			defaults++
		}
	}
	return defaults == 1
}

func validAgentKind(kind string) bool {
	return kind == "dsh" || kind == "codex"
}

func validPeerAddress(value string) bool {
	if len(value) > 128 {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	ip := net.ParseIP(parsed.Hostname())
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || tailscaleIPv4.Contains(ip) || tailscaleIPv6.Contains(ip))
}

func mustCIDR(value string) *net.IPNet {
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		panic(err)
	}
	return network
}
