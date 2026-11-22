package work

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/credentials"
	"github.com/baiyucraft/agent-muster/internal/peer"
)

// 这两个 ldflag 只用于短期实机验证。配置内只有投递、身份及公钥，私钥留在设备。
var rtcBootstrapScope string
var rtcPinnedScope string

const rtcSignatureFooter = "a=x-muster-signature:"

// rtcSigningScope 表示本地发布包预先指定的一笔投递及其对端。
type rtcSigningScope struct {
	transfer string
	local    string
	remote   string
	localKey ed25519.PublicKey
	peerKey  ed25519.PublicKey
}

// rtcPublicKeyReporter 是试验设备可选的公钥登记接口。
type rtcPublicKeyReporter interface {
	ReportRTCPublicKey(context.Context, control.Identity, string) error
}

// signingPrivateKey 在进程内缓存已保存的私钥。读取失败时允许下次重试。
func (transport *rtcTransport) signingPrivateKey() (ed25519.PrivateKey, error) {
	transport.signingKeyMu.Lock()
	defer transport.signingKeyMu.Unlock()
	if transport.signingKey != nil {
		return transport.signingKey, nil
	}
	key, err := loadRTCSigningKey(transport.peer.identity.DeviceID)
	if err != nil {
		return nil, err
	}
	transport.signingKey = key
	return key, nil
}

// reportPublicKey 在引导版中限时登记公开密钥，固定公钥的版本直接使用本机配对策略。
func (transport *rtcTransport) reportPublicKey(ctx context.Context) error {
	if transport.authErr != nil {
		return transport.authErr
	}
	if transport.bootstrap.local == "" || transport.pinned.local != "" {
		return nil
	}
	reporter, ok := transport.hub.(rtcPublicKeyReporter)
	if !ok {
		return nil
	}
	key, err := transport.signingPrivateKey()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return reporter.ReportRTCPublicKey(ctx, transport.peer.identity, base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
}

// signDescription 为本机固定范围内的出站信令添加签名。
func (transport *rtcTransport) signDescription(role, attempt, offer, sdp string) (string, error) {
	key, err := transport.signingPrivateKey()
	if err != nil {
		return "", err
	}
	return signRTCDescription(transport.pinned, key, role, attempt, offer, sdp)
}

// parseRTCSigningScope 解析包内的固定设备与可选公钥，其他设备继续使用原有信令。
func parseRTCSigningScope(raw, local string, pinned bool) (rtcSigningScope, error) {
	if raw == "" {
		return rtcSigningScope{}, nil
	}
	parts := strings.Split(raw, "|")
	count := 3
	second := 2
	if pinned {
		count, second = 5, 3
	}
	if len(parts) != count || !transferID.MatchString(parts[0]) || !validRTCSigningDevice(parts[1]) ||
		!validRTCSigningDevice(parts[second]) || parts[1] == parts[second] {
		return rtcSigningScope{}, errors.New("设备签名信令范围无效")
	}
	scope := rtcSigningScope{transfer: parts[0], local: parts[1], remote: parts[second]}
	if pinned {
		keyA, errA := base64.StdEncoding.Strict().DecodeString(parts[2])
		keyB, errB := base64.StdEncoding.Strict().DecodeString(parts[4])
		if errA != nil || errB != nil || len(keyA) != ed25519.PublicKeySize ||
			len(keyB) != ed25519.PublicKeySize || slices.Equal(keyA, keyB) {
			return rtcSigningScope{}, errors.New("设备签名信令公钥无效")
		}
		scope.localKey, scope.peerKey = keyA, keyB
	}
	switch local {
	case parts[1]:
	case parts[second]:
		scope.local, scope.remote = scope.remote, scope.local
		scope.localKey, scope.peerKey = scope.peerKey, scope.localKey
	default:
		return rtcSigningScope{}, nil
	}
	return scope, nil
}

// validRTCSigningDevice 要求设备 ID 使用固定的十六进制格式，避免其他本机凭据目标。
func validRTCSigningDevice(id string) bool {
	prefix, value, found := strings.Cut(id, "-")
	return found && prefix == "d" && peer.ValidAttempt(value)
}

// loadRTCSigningKey 使用按设备隔离的 Windows 凭据保存种子。只有首次使用才生成。
func loadRTCSigningKey(deviceID string) (key ed25519.PrivateKey, err error) {
	if !validRTCSigningDevice(deviceID) {
		return nil, errors.New("本机签名身份无效")
	}
	store, err := credentials.New("rtc-" + strings.TrimPrefix(deviceID, "d-"))
	if err != nil {
		return nil, err
	}
	err = store.DuringEnrollment(func() error {
		seed, err := store.Load()
		if errors.Is(err, credentials.ErrNotEnrolled) {
			seed = make([]byte, ed25519.SeedSize)
			if _, err := rand.Read(seed); err != nil {
				return err
			}
			if err := store.Save(seed); err != nil {
				clear(seed)
				return err
			}
		} else if err != nil {
			return err
		}
		defer clear(seed)
		if len(seed) != ed25519.SeedSize {
			return errors.New("本机设备签名种子无效")
		}
		key = ed25519.NewKeyFromSeed(seed)
		return nil
	})
	return key, err
}

// rtcSignaturePayload 将角色、双方身份、交接和连接尝试与原始描述绑定。
// 应答额外绑定已签名提议，避免把其他提议的合法应答拼接到当前尝试。
func rtcSignaturePayload(role, sender, receiver, transfer, attempt, offer, sdp string) []byte {
	offerDigest := sha256.Sum256([]byte(offer))
	sdpDigest := sha256.Sum256([]byte(sdp))
	input, _ := json.Marshal([8]string{
		"agent-muster/rtc-sdp/v1", role, sender, receiver, transfer, attempt,
		base64.StdEncoding.EncodeToString(offerDigest[:]), base64.StdEncoding.EncodeToString(sdpDigest[:]),
	})
	return input
}

// signRTCDescription 将签名附加到 SDP 尾部的专用属性，交由 Hub 原样保存。
func signRTCDescription(scope rtcSigningScope, key ed25519.PrivateKey, role, attempt, offer, sdp string) (string, error) {
	if len(key) != ed25519.PrivateKeySize || !scope.localKey.Equal(key.Public()) {
		return "", errors.New("本机签名公钥与发布包指定公钥不匹配")
	}
	if !strings.HasSuffix(sdp, "\r\n") || strings.Contains(sdp, rtcSignatureFooter) {
		return "", errors.New("待签名的设备信令格式无效")
	}
	signature := ed25519.Sign(key, rtcSignaturePayload(role, scope.local, scope.remote, scope.transfer, attempt, offer, sdp))
	signed := sdp + rtcSignatureFooter + base64.StdEncoding.EncodeToString(signature) + "\r\n"
	if !peer.ValidDescription(signed) {
		return "", errors.New("签名后的设备信令超出协议限制")
	}
	return signed, nil
}

// verifyRTCDescription 验证唯一的末尾签名属性，返回交给 WebRTC 的原始 SDP。
func verifyRTCDescription(scope rtcSigningScope, role, attempt, offer, signed string) (string, error) {
	sdp, footer, found := strings.Cut(signed, rtcSignatureFooter)
	if !found || !strings.HasSuffix(sdp, "\r\n") || !strings.HasSuffix(footer, "\r\n") ||
		len(footer) != base64.StdEncoding.EncodedLen(ed25519.SignatureSize)+2 {
		return "", errors.New("缺少有效的对端设备信令签名")
	}
	encoded := strings.TrimSuffix(footer, "\r\n")
	signature, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || !peer.ValidDescription(sdp) || len(scope.peerKey) != ed25519.PublicKeySize ||
		!ed25519.Verify(scope.peerKey, rtcSignaturePayload(role, scope.remote, scope.local, scope.transfer, attempt, offer, sdp), signature) {
		return "", errors.New("对端设备信令签名校验失败")
	}
	return sdp, nil
}
