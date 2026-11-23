package httpapi

import (
	"net/http"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func (api workspaceAPI) bindingCandidates(w http.ResponseWriter, r *http.Request) {
	candidates, err := api.store.BindingCandidates(r.Context())
	if candidates == nil {
		candidates = []workspace.BindingCandidate{}
	}
	workspaceJSON(w, http.StatusOK, struct {
		Candidates []workspace.BindingCandidate `json:"candidates"`
	}{candidates}, err)
}

func (api workspaceAPI) proposeBinding(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var request struct {
		Lease    string `json:"lease"`
		Name     string `json:"name"`
		Bindings []struct {
			DeviceID    string `json:"device_id"`
			DirectoryID string `json:"directory_id"`
			AgentKind   string `json:"agent_kind"`
			IsDefault   *bool  `json:"is_default"`
		} `json:"bindings"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if !prefixedUUID(id, "message-") || !leasePattern.MatchString(request.Lease) {
		workspaceJSON(w, 0, nil, workspace.ErrInvalidBindingProposal)
		return
	}
	input := workspace.BindingProposalInput{Lease: request.Lease, Name: request.Name}
	for _, binding := range request.Bindings {
		if binding.IsDefault == nil {
			workspaceJSON(w, 0, nil, workspace.ErrInvalidBindingProposal)
			return
		}
		input.Bindings = append(input.Bindings, workspace.ProposedBinding{
			DeviceID: binding.DeviceID, DirectoryID: binding.DirectoryID, AgentKind: binding.AgentKind, IsDefault: *binding.IsDefault,
		})
	}
	proposal, err := api.store.ProposeBinding(r.Context(), id, input)
	workspaceJSON(w, http.StatusOK, proposal, err)
}

func (api workspaceAPI) decideBinding(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var input struct {
		Decision string `json:"decision"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !prefixedUUID(id, "proposal-") || (input.Decision != "accepted" && input.Decision != "rejected") {
		workspaceJSON(w, 0, nil, workspace.ErrInvalidBindingProposal)
		return
	}
	proposal, err := api.store.DecideBinding(r.Context(), id, input.Decision)
	workspaceJSON(w, http.StatusOK, proposal, err)
}
