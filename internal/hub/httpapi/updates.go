package httpapi

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/baiyucraft/agent-muster/internal/deviceupdate"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
)

type updateStore interface {
	ListDeviceUpdates(context.Context) ([]deviceupdate.DeviceState, error)
	AssignDeviceUpdate(context.Context, string, string, deviceupdate.Assignment) (deviceupdate.DeviceState, error)
	CheckDeviceUpdate(context.Context, string, [32]byte, deviceupdate.CheckRequest) (*deviceupdate.Target, error)
}

type updateAPI struct {
	store updateStore
}

// registerUpdates 用管理员凭据保护下发操作，用设备凭据绑定轮询和回执。
func (s server) registerUpdates(mux *http.ServeMux) {
	store, ok := s.store.(updateStore)
	if !ok {
		return
	}
	api := updateAPI{store: store}
	mux.HandleFunc("GET /api/v1/device-updates", s.admin(api.list))
	mux.HandleFunc("PUT /api/v1/device-updates/{device_id}", s.admin(api.assign))
	mux.HandleFunc("POST /api/v1/devices/{device_id}/update/check", s.device(api.check))
}

func (api updateAPI) list(w http.ResponseWriter, r *http.Request) {
	devices, err := api.store.ListDeviceUpdates(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Devices []deviceupdate.DeviceState `json:"devices"`
	}{Devices: devices})
}

func (api updateAPI) assign(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("device_id")
	if !registry.ValidDeviceID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	var assignment deviceupdate.Assignment
	if !readJSON(w, r, &assignment) {
		return
	}
	if err := assignment.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Hub 保存签名清单地址，设备负责下载和验证发布签名。
	id, err := randomBytes(16)
	if err != nil {
		storeError(w, err)
		return
	}
	state, err := api.store.AssignDeviceUpdate(r.Context(), deviceID, "upd-"+hex.EncodeToString(id), assignment)
	if errors.Is(err, deviceupdate.ErrConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (api updateAPI) check(w http.ResponseWriter, r *http.Request, deviceID string, token [32]byte) {
	var check deviceupdate.CheckRequest
	if !readJSON(w, r, &check) {
		return
	}
	if err := check.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	target, err := api.store.CheckDeviceUpdate(r.Context(), deviceID, token, check)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deviceupdate.CheckResponse{Target: target})
}
