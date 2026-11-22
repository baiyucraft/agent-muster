package work

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/peer"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

const (
	rtcLabel         = "muster-transfer-v1"
	rtcChunkBytes    = 16 << 10
	rtcOfferTimeout  = 35 * time.Second
	rtcBodyTimeout   = 90 * time.Second
	rtcPollInterval  = time.Second
	rtcRefresh       = 30 * time.Second
	rtcGatherTimeout = 5 * time.Second
)

type rtcHub interface {
	RegisterRTC(context.Context, control.Identity) (peer.RTCConfig, error)
	RTCOffers(context.Context, control.Identity) ([]peer.RTCOffer, error)
	PutRTCOffer(context.Context, control.Identity, string, peer.RTCSignal) error
	RTCAnswer(context.Context, control.Identity, string, string) (peer.RTCAnswer, error)
	PutRTCAnswer(context.Context, control.Identity, string, peer.RTCSignal) error
}

type rtcStream interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// rtcTransport 通过 Hub 交换 ICE 候选与 DTLS 指纹，使用设备间的有序数据通道传送正文。
type rtcTransport struct {
	peer     *collaboration
	hub      rtcHub
	api      *webrtc.API
	onStatus func(string)

	mu   sync.RWMutex
	ice  []webrtc.ICEServer
	live bool
}

func newRTCTransport(collaboration *collaboration, hub rtcHub, onStatus func(string)) *rtcTransport {
	settings := webrtc.SettingEngine{}
	// 每块正文对应一条数据通道消息，阻塞写入为待发送数据提供背压。
	settings.DetachDataChannels()
	settings.EnableDataChannelBlockWrite(true)
	settings.SetSCTPMaxMessageSize(rtcChunkBytes)
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	if origin, err := url.Parse(collaboration.identity.HubURL); err == nil {
		settings.SetIncludeLoopbackCandidate(origin.Hostname() == "localhost" || net.ParseIP(origin.Hostname()).IsLoopback())
	}
	settings.SetICETimeouts(5*time.Second, 10*time.Second, 2*time.Second)
	return &rtcTransport{
		peer: collaboration, hub: hub, onStatus: onStatus,
		api: webrtc.NewAPI(webrtc.WithSettingEngine(settings)),
	}
}

func (transport *rtcTransport) status(status string) {
	if transport.onStatus != nil {
		transport.onStatus(status)
	}
}

func (transport *rtcTransport) listen(ctx context.Context, onError func(error)) {
	ctx, cancel := context.WithCancel(ctx)
	transport.status("connecting")
	var incoming <-chan struct{}
	defer func() {
		cancel()
		if incoming != nil {
			<-incoming
		}
	}()
	var refreshAfter time.Time
	registered := false
	retry := time.Second
	for ctx.Err() == nil {
		if !time.Now().Before(refreshAfter) {
			err := transport.register(ctx)
			switch {
			case err == nil:
				if !registered {
					transport.status("ready")
					registered = true
				}
				retry = time.Second
				refreshAfter = time.Now().Add(rtcRefresh)
			case errors.Is(err, control.ErrUnauthorized):
				transport.status("unavailable")
				return
			default:
				transport.status("unavailable")
				registered = false
				if transport.peer.address == "" && onError != nil && ctx.Err() == nil {
					onError(err)
				}
				if !sleepContext(ctx, retry) {
					return
				}
				retry = min(2*retry, rtcRefresh)
				continue
			}
		}
		select {
		case <-incoming:
			incoming = nil
		default:
		}
		// 每次只接收一个投递，长传输在独立 goroutine 中进行，避免占用信令续期循环。
		if incoming == nil {
			offers, err := transport.hub.RTCOffers(ctx, transport.peer.identity)
			if errors.Is(err, control.ErrUnauthorized) {
				transport.status("unavailable")
				return
			}
			if err == nil && len(offers) != 0 {
				done := make(chan struct{})
				incoming = done
				go func(offer peer.RTCOffer) {
					defer close(done)
					if err := transport.receive(ctx, offer); err != nil && ctx.Err() == nil && onError != nil {
						onError(err)
					}
				}(offers[0])
			}
		}
		if !sleepContext(ctx, rtcPollInterval) {
			return
		}
	}
}

func (transport *rtcTransport) register(ctx context.Context) error {
	config, err := transport.hub.RegisterRTC(ctx, transport.peer.identity)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.live = err == nil
	if err != nil {
		return err
	}
	transport.ice = make([]webrtc.ICEServer, 0, len(config.ICEServers))
	for _, server := range config.ICEServers {
		transport.ice = append(transport.ice, webrtc.ICEServer{URLs: server.URLs, Username: server.Username, Credential: server.Credential})
	}
	return nil
}

func (transport *rtcTransport) connection() (*webrtc.PeerConnection, error) {
	transport.mu.RLock()
	live := transport.live
	servers := transport.ice
	transport.mu.RUnlock()
	if !live {
		return nil, errors.New("设备信令尚未登记")
	}
	return transport.api.NewPeerConnection(webrtc.Configuration{ICEServers: servers})
}

func localDescription(ctx context.Context, connection *webrtc.PeerConnection, description webrtc.SessionDescription) (string, error) {
	gathered := webrtc.GatheringCompletePromise(connection)
	if err := connection.SetLocalDescription(description); err != nil {
		return "", err
	}
	timer := time.NewTimer(rtcGatherTimeout)
	defer timer.Stop()
	// 信令一次提交当前可用的候选。候选收集超时后仍可使用已取得的候选连接。
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
	case <-gathered:
	}
	local := connection.LocalDescription()
	if local == nil || !peer.ValidDescription(local.SDP) {
		return "", errors.New("设备数据通道描述无效")
	}
	return local.SDP, nil
}

func publishRTCStream(channel *webrtc.DataChannel, ready chan<- rtcStream) {
	if channel.Label() != rtcLabel || !channel.Ordered() || channel.MaxRetransmits() != nil || channel.MaxPacketLifeTime() != nil {
		_ = channel.Close()
		return
	}
	channel.OnOpen(func() {
		stream, err := channel.DetachWithDeadline()
		if err != nil {
			return
		}
		select {
		case ready <- stream:
		default:
			_ = stream.Close()
		}
	})
}

func waitRTCStream(ctx context.Context, ready <-chan rtcStream) (rtcStream, error) {
	select {
	case stream := <-ready:
		return stream, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func rtcDeadline(stream rtcStream) error {
	deadline := time.Now().Add(rtcBodyTimeout)
	return errors.Join(stream.SetReadDeadline(deadline), stream.SetWriteDeadline(deadline))
}

func (transport *rtcTransport) push(ctx context.Context, manifest control.Transfer, body []byte) error {
	if !transferID.MatchString(manifest.ID) || len(manifest.Ticket) != 64 ||
		int64(len(body)) != manifest.Size || transferHash(body) != manifest.SHA256 {
		return errors.New("待发正文与投递记录不匹配")
	}
	handshake, done := context.WithTimeout(ctx, rtcOfferTimeout)
	defer done()
	connection, err := transport.connection()
	if err != nil {
		return err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	channel, err := connection.CreateDataChannel(rtcLabel, nil)
	if err != nil {
		return err
	}
	ready := make(chan rtcStream, 1)
	publishRTCStream(channel, ready)
	offer, err := connection.CreateOffer(nil)
	if err != nil {
		return err
	}
	sdp, err := localDescription(handshake, connection, offer)
	if err != nil {
		return err
	}
	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return err
	}
	signal := peer.RTCSignal{AttemptID: hex.EncodeToString(randomID[:]), SDP: sdp}
	if err := transport.hub.PutRTCOffer(handshake, transport.peer.identity, manifest.ID, signal); err != nil {
		return err
	}
	answer, err := transport.waitAnswer(handshake, manifest.ID, signal.AttemptID)
	if err != nil {
		return err
	}
	if err := connection.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		return err
	}
	stream, err := waitRTCStream(handshake, ready)
	if err != nil {
		return err
	}
	if err := rtcDeadline(stream); err != nil {
		return err
	}
	transport.selectedPath(connection)
	// 通过已建立的 DTLS 数据通道发送投递凭据，目标设备验证后才发送正文。
	if err := writeRTCPacket(stream, []byte(manifest.Ticket)); err != nil {
		return err
	}
	if err := expectRTCPacket(stream, "ready"); err != nil {
		return err
	}
	for len(body) != 0 {
		length := min(len(body), rtcChunkBytes)
		if err := writeRTCPacket(stream, body[:length]); err != nil {
			return err
		}
		body = body[length:]
	}
	if err := expectRTCPacket(stream, "saved"); err != nil {
		return err
	}
	// saved 表示收件已持久化且 Hub 已确认。seen 让接收方等到收件回执抵达后再关闭通道。
	_ = writeRTCPacket(stream, []byte("seen"))
	return nil
}

func (transport *rtcTransport) waitAnswer(ctx context.Context, transfer, attempt string) (string, error) {
	for {
		answer, err := transport.hub.RTCAnswer(ctx, transport.peer.identity, transfer, attempt)
		if err != nil {
			return "", err
		}
		if answer.SDP != "" {
			if !peer.ValidDescription(answer.SDP) {
				return "", errors.New("接收设备信令答复无效")
			}
			return answer.SDP, nil
		}
		if !sleepContext(ctx, 500*time.Millisecond) {
			return "", ctx.Err()
		}
	}
}

func (transport *rtcTransport) receive(ctx context.Context, offer peer.RTCOffer) error {
	if !transferID.MatchString(offer.TransferID) || !peer.ValidAttempt(offer.AttemptID) || !peer.ValidDescription(offer.SDP) {
		return errors.New("发送设备信令无效")
	}
	handshake, done := context.WithTimeout(ctx, rtcOfferTimeout)
	defer done()
	connection, err := transport.connection()
	if err != nil {
		return err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	ready := make(chan rtcStream, 1)
	connection.OnDataChannel(func(channel *webrtc.DataChannel) { publishRTCStream(channel, ready) })
	if err := connection.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		return err
	}
	answer, err := connection.CreateAnswer(nil)
	if err != nil {
		return err
	}
	sdp, err := localDescription(handshake, connection, answer)
	if err != nil {
		return err
	}
	signal := peer.RTCSignal{AttemptID: offer.AttemptID, SDP: sdp}
	if err := transport.hub.PutRTCAnswer(handshake, transport.peer.identity, offer.TransferID, signal); err != nil {
		var status *control.HTTPError
		// 并发重试或过期会触发冲突，旧尝试可由后续轮询接手。
		if errors.As(err, &status) && status.Status == http.StatusConflict {
			return nil
		}
		return err
	}
	stream, err := waitRTCStream(handshake, ready)
	if err != nil {
		return err
	}
	if err := rtcDeadline(stream); err != nil {
		return err
	}
	transport.selectedPath(connection)
	return transport.receiveBody(ctx, stream, offer.TransferID)
}

func (transport *rtcTransport) receiveBody(ctx context.Context, stream rtcStream, id string) error {
	packet := make([]byte, rtcChunkBytes)
	length, err := stream.Read(packet)
	if err != nil {
		return err
	}
	if length != 64 {
		return errors.New("设备投递授权无效")
	}
	ticket := string(packet[:length])
	manifest, err := transport.peer.verifyInbound(ctx, id, ticket)
	if err != nil {
		return err
	}
	if err := writeRTCPacket(stream, []byte("ready")); err != nil {
		return err
	}
	body := make([]byte, 0, manifest.Size)
	for int64(len(body)) < manifest.Size {
		length, err := stream.Read(packet)
		if err != nil {
			return err
		}
		if length == 0 || int64(len(body)+length) > manifest.Size {
			return errInboundDigest
		}
		body = append(body, packet[:length]...)
	}
	if err := transport.peer.commitInbound(ctx, manifest, ticket, body); err != nil {
		return err
	}
	// saved 在本地收件落盘及 Hub 确认之后发送。seen 丢失时已有的确认仍然有效。
	if err := writeRTCPacket(stream, []byte("saved")); err != nil {
		return err
	}
	_ = stream.SetReadDeadline(time.Now().Add(2 * time.Second))
	_ = expectRTCPacket(stream, "seen")
	return nil
}

func writeRTCPacket(stream io.Writer, bytes []byte) error {
	length, err := stream.Write(bytes)
	if err != nil {
		return err
	}
	if length != len(bytes) {
		return io.ErrShortWrite
	}
	return nil
}

func expectRTCPacket(stream io.Reader, message string) error {
	var packet [16]byte
	length, err := stream.Read(packet[:])
	if err != nil {
		return err
	}
	if string(packet[:length]) != message {
		return errors.New("设备数据通道的应答无效")
	}
	return nil
}

func (transport *rtcTransport) selectedPath(connection *webrtc.PeerConnection) {
	pair, err := connection.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return
	}
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		transport.status("relay")
		return
	}
	transport.status("direct")
}
