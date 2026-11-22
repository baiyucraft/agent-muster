package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"os"
)

type rtcPublicKeyStore interface {
	SaveRTCPublicKey(context.Context, string, [32]byte, string, []byte) error
}

// reportRTCPublicKey 接收指定验收投递参与设备的首次公钥声明。已记录的公钥不可覆盖。
func (api workspaceAPI) reportRTCPublicKey(w http.ResponseWriter, r *http.Request, device string, token [32]byte) {
	transfer := os.Getenv("MUSTER_RTC_KEY_TRANSFER_ID")
	store, enabled := api.store.(rtcPublicKeyStore)
	if !enabled || !prefixedUUID(transfer, "transfer-") {
		writeError(w, http.StatusNotFound, "device signing key enrollment unavailable")
		return
	}
	var request struct {
		PublicKey string `json:"public_key"`
	}
	if !readJSONLimit(w, r, &request, 128) {
		return
	}
	key, err := base64.StdEncoding.Strict().DecodeString(request.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(key) != request.PublicKey {
		writeError(w, http.StatusBadRequest, "invalid Ed25519 public key")
		return
	}
	workspaceJSON(w, http.StatusNoContent, nil, store.SaveRTCPublicKey(r.Context(), device, token, transfer, key))
}
