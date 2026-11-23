package httpapi

import (
	"net/http"

	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
)

func (api workspaceAPI) authorizeGitPush(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	var input gitdelivery.PushRequest
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !prefixedUUID(id, "run-") || !validRunLease(input.Generation, input.Lease) {
		writeError(w, http.StatusBadRequest, "invalid Git push request")
		return
	}
	value, err := api.store.AuthorizeGitPush(r.Context(), device, token, id, input)
	workspaceJSON(w, http.StatusOK, value, err)
}

func (api workspaceAPI) observeGitPush(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	var input gitdelivery.Observation
	if !readJSON(w, r, &input) {
		return
	}
	id, intentID := r.PathValue("id"), r.PathValue("intent_id")
	if !prefixedUUID(id, "run-") || !prefixedUUID(intentID, "push-") || !validRunLease(input.Generation, input.Lease) {
		writeError(w, http.StatusBadRequest, "invalid Git push observation")
		return
	}
	value, err := api.store.ObserveGitPush(r.Context(), device, token, id, intentID, input)
	workspaceJSON(w, http.StatusOK, value, err)
}
