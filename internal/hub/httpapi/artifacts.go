package httpapi

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/baiyucraft/agent-muster/internal/artifact"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

const artifactReadTimeout = 45 * time.Second

// artifactAPI 按管理员请求向持有设备读取一个已登记快照，正文只在本次 HTTP 请求存续期间中转。
type artifactAPI struct {
	store   workspace.Store
	mu      sync.Mutex
	pending *pendingArtifactRead
}

type pendingArtifactRead struct {
	request   artifact.ReadRequest
	ref       workspace.TaskArtifact
	ctx       context.Context
	response  chan artifactReply
	uploading bool
	complete  bool
	readDone  bool
}

type artifactReply struct {
	status int
	body   []byte
}

func (api *artifactAPI) read(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "artifact reads require GET")
		return
	}
	runID, id := r.PathValue("id"), r.PathValue("artifact_id")
	if !prefixedUUID(runID, "run-") || !artifact.ValidID(id) {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	ref, err := api.store.Artifact(r.Context(), runID, id)
	if err != nil {
		storeError(w, err)
		return
	}
	device, err := api.store.RunLogDevice(r.Context(), ref.TaskID, runID)
	if err != nil {
		if errors.Is(err, workspace.ErrRunLogDeviceOffline) || errors.Is(err, workspace.ErrRunLogDeviceRevoked) || errors.Is(err, workspace.ErrRunLogsUnavailable) {
			writeError(w, http.StatusConflict, err.Error())
		} else {
			storeError(w, err)
		}
		return
	}
	value, err := randomBytes(16)
	if err != nil {
		storeError(w, err)
		return
	}
	if ref.ID != id || ref.RunID != runID || ref.DeviceID != device || !artifact.ValidFor(runID, []artifact.Reference{ref.Reference}) {
		writeError(w, http.StatusBadGateway, "invalid stored artifact reference")
		return
	}
	pending := &pendingArtifactRead{
		request: artifact.ReadRequest{ID: "artifact-" + hex.EncodeToString(value), RunID: runID, ArtifactID: id},
		ref:     ref, ctx: r.Context(), response: make(chan artifactReply, 1),
	}
	api.mu.Lock()
	if api.pending != nil {
		api.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "another artifact read is active")
		return
	}
	api.pending = pending
	api.mu.Unlock()
	defer func() {
		api.mu.Lock()
		pending.readDone = true
		if !pending.uploading {
			api.pending = nil
		}
		api.mu.Unlock()
	}()
	controller := http.NewResponseController(w)
	if deadline, ok := r.Context().Deadline(); ok {
		// 下载正文仍受有界写入时间限制，避免慢读取方占住全局槽位。
		_ = controller.SetWriteDeadline(deadline.Add(15 * time.Second))
	}
	select {
	case <-r.Context().Done():
		if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "execution device did not respond to the artifact request")
		}
	case reply := <-pending.response:
		if reply.status == http.StatusNotFound {
			writeError(w, reply.status, "artifact snapshot is missing on the device")
			return
		}
		if reply.status != http.StatusOK {
			writeError(w, reply.status, "artifact snapshot verification failed")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(ref.Path)}))
		w.Header().Set("Content-Length", strconv.Itoa(len(reply.body)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(reply.body)
	}
}

func (api *artifactAPI) list(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	if err := api.store.AuthenticateRunLogDevice(r.Context(), device, token); err != nil {
		storeError(w, err)
		return
	}
	api.mu.Lock()
	pending := api.pending
	var request artifact.ReadRequest
	if pending != nil && pending.ref.DeviceID == device && !pending.uploading && !pending.complete && pending.ctx.Err() == nil {
		request = pending.request
	}
	api.mu.Unlock()
	if request.ID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, request)
}

func (api *artifactAPI) submit(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	if err := api.store.AuthenticateRunLogDevice(r.Context(), device, token); err != nil {
		storeError(w, err)
		return
	}
	api.mu.Lock()
	pending := api.pending
	if pending == nil || pending.ref.DeviceID != device || pending.request.ID != r.PathValue("request_id") ||
		pending.ctx.Err() != nil || pending.complete {
		api.mu.Unlock()
		writeError(w, http.StatusNotFound, "artifact request is unavailable")
		return
	}
	if pending.uploading {
		api.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "an artifact upload is active")
		return
	}
	pending.uploading = true
	api.mu.Unlock()
	defer func() {
		api.mu.Lock()
		pending.uploading = false
		if pending.readDone {
			api.pending = nil
		}
		api.mu.Unlock()
	}()
	if deadline, ok := pending.ctx.Deadline(); ok {
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(deadline)
		_ = controller.SetWriteDeadline(deadline.Add(time.Second))
	}
	reply, ok := readArtifactUpload(w, r, pending.ref.Reference)
	if !ok {
		return
	}
	// 正文完整接收后再鉴权，计算摘要期间撤销的设备不能提交结果。
	if err := api.store.AuthenticateRunLogDevice(r.Context(), device, token); err != nil {
		storeError(w, err)
		return
	}
	api.mu.Lock()
	if pending.ctx.Err() != nil {
		api.mu.Unlock()
		writeError(w, http.StatusNotFound, "artifact request is unavailable")
		return
	}
	pending.complete = true
	pending.response <- reply
	api.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func readArtifactUpload(w http.ResponseWriter, r *http.Request, ref artifact.Reference) (artifactReply, bool) {
	status := r.URL.Query().Get("status")
	switch status {
	case "available":
		if r.Header.Get("Content-Type") != "application/octet-stream" {
			writeError(w, http.StatusBadRequest, "artifact upload requires application/octet-stream")
			return artifactReply{}, false
		}
		if r.ContentLength > artifact.MaxSize {
			writeError(w, http.StatusRequestEntityTooLarge, "artifact snapshot exceeds the byte limit")
			return artifactReply{}, false
		}
		// 只读取登记大小和额外一个字节，既能校验分块上传又避免为未知长度扩容。
		body := make([]byte, int(ref.Size)+1)
		n, err := io.ReadFull(r.Body, body)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			writeError(w, http.StatusBadRequest, "artifact upload is incomplete")
			return artifactReply{}, false
		}
		if n > artifact.MaxSize {
			writeError(w, http.StatusRequestEntityTooLarge, "artifact snapshot exceeds the byte limit")
			return artifactReply{}, false
		}
		body = body[:n]
		if !ref.Matches(body) {
			return artifactReply{status: http.StatusBadGateway}, true
		}
		return artifactReply{status: http.StatusOK, body: body}, true
	case "missing", "failed":
		if r.ContentLength != 0 {
			writeError(w, http.StatusBadRequest, "artifact failure status requires an empty body")
			return artifactReply{}, false
		}
		if status == "missing" {
			return artifactReply{status: http.StatusNotFound}, true
		}
		return artifactReply{status: http.StatusBadGateway}, true
	default:
		writeError(w, http.StatusBadRequest, "invalid artifact upload status")
		return artifactReply{}, false
	}
}
