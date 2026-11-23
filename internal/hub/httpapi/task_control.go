package httpapi

import (
	"net/http"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

func (api workspaceAPI) controlTask(w http.ResponseWriter, r *http.Request) {
	var input workspace.TaskControl
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !prefixedUUID(id, "task-") || !uuidPattern.MatchString(input.RequestID) {
		writeError(w, http.StatusBadRequest, "invalid task or control request identity")
		return
	}
	switch input.Action {
	case "pause", "stop_current", "resume", "cancel":
	default:
		writeError(w, http.StatusBadRequest, "unknown task control action")
		return
	}
	value, err := api.store.ControlTask(r.Context(), id, input)
	workspaceJSON(w, http.StatusOK, value, err)
}

func (api workspaceAPI) runControl(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	var input struct {
		Generation int64  `json:"generation"`
		Lease      string `json:"lease"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("id")
	if !prefixedUUID(id, "run-") || !validRunLease(input.Generation, input.Lease) {
		writeError(w, http.StatusBadRequest, "invalid run or lease")
		return
	}
	stop, err := api.store.RunControl(r.Context(), device, token, id, input.Generation, input.Lease)
	workspaceJSON(w, http.StatusOK, struct {
		Stop bool `json:"stop"`
	}{Stop: stop}, err)
}
