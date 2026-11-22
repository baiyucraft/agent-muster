// Package peer 定义设备通过 Hub 交换的 WebRTC 信令。投递正文经设备间的数据通道传送。
package peer

import (
	"regexp"
	"strings"
)

const (
	// MaxSDPBytes 限制单条 SDP 的字节数。
	MaxSDPBytes = 12 << 10
	// OfferLimit 限制设备单次轮询可取回的提议数量。
	OfferLimit = 4
)

var attemptPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidAttempt 检查连接尝试 ID 是否为 32 位小写十六进制文本。
func ValidAttempt(id string) bool {
	return attemptPattern.MatchString(id)
}

// ValidDescription 检查 SDP 的长度、候选数量、DTLS 指纹属性和数据通道类型。
// 连接建立时由 WebRTC 核对对端证书与信令中的指纹。
func ValidDescription(description string) bool {
	if len(description) > MaxSDPBytes || !strings.HasPrefix(description, "v=0\r\n") ||
		strings.Count(description, "\nm=") != 1 || strings.Count(description, "\na=candidate:") > 32 ||
		!strings.Contains(description, "\na=fingerprint:sha-256 ") {
		return false
	}
	_, media, _ := strings.Cut(description, "\nm=")
	line, _, _ := strings.Cut(media, "\r\n")
	fields := strings.Fields(line)
	return len(fields) == 4 && fields[0] == "application" &&
		fields[2] == "UDP/DTLS/SCTP" && fields[3] == "webrtc-datachannel"
}

// ICEServer 保存 STUN 或 TURN 地址，TURN 还使用用户名和凭据。
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// RTCConfig 是 Hub 在设备登记成功后下发的 ICE 配置。
type RTCConfig struct {
	ICEServers []ICEServer `json:"ice_servers"`
}

// RTCSignal 将 SDP 绑定到单次连接尝试。重连使用新的 AttemptID。
type RTCSignal struct {
	AttemptID string `json:"attempt_id"`
	SDP       string `json:"sdp"`
}

// RTCOffer 将连接提议关联到持久投递 ID，由目标设备读取。
type RTCOffer struct {
	TransferID string `json:"transfer_id"`
	RTCSignal
}

// RTCOffers 是目标设备一次轮询取回的待应答连接提议。
type RTCOffers struct {
	Offers []RTCOffer `json:"offers"`
}

// RTCAnswer 返回连接应答，SDP 为空时表示目标设备尚未应答。
type RTCAnswer struct {
	SDP string `json:"sdp"`
}
