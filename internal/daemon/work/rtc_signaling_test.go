package work

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/peer"
	"github.com/google/uuid"
	"github.com/pion/turn/v5"
	"github.com/pion/webrtc/v4"
)

func TestRTCTrialDirectServers(t *testing.T) {
	servers := []webrtc.ICEServer{
		{URLs: []string{"stun:stun.example.org:3478"}},
		{URLs: []string{"turn:relay.example.org:3478", "turns:relay.example.org:5349"}, Username: "trial", Credential: "fixture"},
	}
	want := servers[:1]
	if got := stunServers(servers); !reflect.DeepEqual(got, want) {
		t.Fatalf("试点之外只使用 STUN 服务，实际为 %v", got)
	}
}

func TestRTCSignedSignaling(t *testing.T) {
	pubA, keyA, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB, keyB, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const (
		deviceA  = "d-11111111111111111111111111111111"
		deviceB  = "d-22222222222222222222222222222222"
		transfer = "transfer-71873540-3b5b-4f69-a81d-a4f348373cb4"
		attempt  = "abcdef0123456789abcdef0123456789"
		sdp      = "v=0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=fingerprint:sha-256 AA:BB\r\n"
	)
	pinned := strings.Join([]string{transfer, deviceA, base64.StdEncoding.EncodeToString(pubA), deviceB, base64.StdEncoding.EncodeToString(pubB)}, "|")
	sender, err := parseRTCSigningScope(pinned, deviceA, true)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := parseRTCSigningScope(pinned, deviceB, true)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := signRTCDescription(sender, keyA, "offer", attempt, "", sdp)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := verifyRTCDescription(receiver, "offer", attempt, "", offer); err != nil || got != sdp {
		t.Fatalf("接收方应取回已验签的原始提议，得到 %v", err)
	}
	answer, err := signRTCDescription(receiver, keyB, "answer", attempt, offer, sdp)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := verifyRTCDescription(sender, "answer", attempt, offer, answer); err != nil || got != sdp {
		t.Fatalf("发送方应取回已验签的原始应答，得到 %v", err)
	}
	for name, invalid := range map[string]string{
		"unsigned":         sdp,
		"fingerprint":      strings.Replace(offer, "AA:BB", "BB:AA", 1),
		"signature_footer": offer + "a=ice-options:trickle\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifyRTCDescription(receiver, "offer", attempt, "", invalid); err == nil {
				t.Fatal("应拒绝篡改或缺少签名的信令")
			}
		})
	}
	if _, err := verifyRTCDescription(receiver, "answer", attempt, "", offer); err == nil {
		t.Fatal("签名应绑定信令角色")
	}
	if _, err := verifyRTCDescription(receiver, "offer", "00000000000000000000000000000000", "", offer); err == nil {
		t.Fatal("签名应绑定连接尝试")
	}
	if _, err := verifyRTCDescription(sender, "answer", attempt, sdp, answer); err == nil {
		t.Fatal("应答应绑定已签名提议")
	}
	receiver.transfer = "transfer-1d925bc0-ec4b-4b8e-931e-51cf5aa28732"
	if _, err := verifyRTCDescription(receiver, "offer", attempt, "", offer); err == nil {
		t.Fatal("签名应绑定投递")
	}
}

type testRTCHub struct {
	rtcHub
	testTransferHub
	config peer.RTCConfig
	signed bool
	offer  chan peer.RTCOffer
	answer chan peer.RTCAnswer
}

func (hub *testRTCHub) RegisterRTC(context.Context, control.Identity) (peer.RTCConfig, error) {
	return hub.config, nil
}

func (hub *testRTCHub) PutRTCOffer(_ context.Context, _ control.Identity, transfer string, signal peer.RTCSignal) error {
	if strings.Contains(signal.SDP, rtcSignatureFooter) != hub.signed {
		return errors.New("连接提议的签名状态与测试场景不符")
	}
	hub.offer <- peer.RTCOffer{TransferID: transfer, RTCSignal: signal}
	return nil
}

func (hub *testRTCHub) PutRTCAnswer(_ context.Context, _ control.Identity, _ string, signal peer.RTCSignal) error {
	if strings.Contains(signal.SDP, rtcSignatureFooter) != hub.signed {
		return errors.New("连接应答的签名状态与测试场景不符")
	}
	hub.answer <- peer.RTCAnswer{SDP: signal.SDP}
	return nil
}

func (hub *testRTCHub) RTCAnswer(ctx context.Context, _ control.Identity, _, _ string) (peer.RTCAnswer, error) {
	select {
	case answer := <-hub.answer:
		return answer, nil
	case <-ctx.Done():
		return peer.RTCAnswer{}, ctx.Err()
	}
}

func TestRTCSignedTransfer(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		testRTCTransfers(t, "direct", nil, true, 1)
	})
	t.Run("relay", func(t *testing.T) {
		server := startTestTURN(t)
		testRTCTransfers(t, "relay", &server, true, 1)
	})
}

func TestRTCStandardTransfersUseTURN(t *testing.T) {
	server := startTestTURN(t)
	testRTCTransfers(t, "relay", &server, false, 2)
}

const testTURNRelayAddress = "127.0.0.2"

// startTestTURN 启动只接受本机流量的临时 TURN 服务器。
func startTestTURN(t *testing.T) peer.ICEServer {
	t.Helper()
	const realm, username, password = "muster.test", "muster", "test-credential"
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	key := turn.GenerateAuthKey(username, realm, password)
	server, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		AuthHandler: func(request *turn.RequestAttributes) (string, []byte, bool) {
			return username, key, request.Username == username && request.Realm == realm
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: listener,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: net.ParseIP(testTURNRelayAddress), Address: testTURNRelayAddress,
			},
			PermissionHandler: func(_ net.Addr, ip net.IP) bool {
				return ip.IsLoopback()
			},
		}},
	})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return peer.ICEServer{
		URLs: []string{"turn:" + listener.LocalAddr().String() + "?transport=udp"}, Username: username, Credential: password,
	}
}

// testRTCTransfers 用真实本机数据通道验证选路、收件落盘和向模拟 Hub 确认。
func testRTCTransfers(t *testing.T, wantPath string, relay *peer.ICEServer, signed bool, count int) {
	t.Helper()
	const (
		deviceA = "d-11111111111111111111111111111111"
		deviceB = "d-22222222222222222222222222222222"
	)
	hub := &testRTCHub{signed: signed, offer: make(chan peer.RTCOffer, 1), answer: make(chan peer.RTCAnswer, 1)}
	if relay != nil {
		hub.config.ICEServers = []peer.ICEServer{*relay}
	}
	// 跳过共用投递夹具用于注入丢失回执的首次调用。
	hub.ackCalls.Store(1)
	open := func(local string) (*collaboration, *string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		worker, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = worker.Close() })
		var selected string
		device, err := openPeer(ctx, worker, hub, control.Identity{DeviceID: local, HubURL: "http://127.0.0.1"}, PeerConfig{
			OnStatus: func(status string) { selected = status },
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := device.rtc.register(ctx); err != nil {
			t.Fatal(err)
		}
		if relay != nil {
			// 只接收远端中继地址，模拟设备间无法直连。连接仍使用普通 ICE 策略。
			settings := rtcSettings(device.identity.HubURL)
			relayIP := net.ParseIP(testTURNRelayAddress)
			settings.SetRemoteIPFilter(func(ip net.IP) bool { return ip.Equal(relayIP) })
			device.rtc.api = webrtc.NewAPI(webrtc.WithSettingEngine(settings))
		}
		return device, &selected
	}
	sender, senderPath := open(deviceA)
	receiver, receiverPath := open(deviceB)
	for index := range count {
		transfer := "transfer-" + uuid.NewString()
		body := []byte("fixture " + transfer)
		hub.manifest = control.Transfer{
			ID: transfer, SourceDeviceID: deviceA, TargetDeviceID: deviceB,
			Name: "fixture", Kind: "message", Size: int64(len(body)), SHA256: transferHash(body), Ticket: strings.Repeat("a", 64),
		}
		if signed {
			pinRTCTransfer(t, sender, receiver, transfer)
			hub.manifest.Peer = &control.Peer{DeviceID: deviceB, Address: "not-used"}
		}
		hub.received.Store(false)
		*senderPath, *receiverPath = "", ""
		checkRTCTransfer(t, hub, sender, receiver, body)
		if *senderPath != wantPath || *receiverPath != wantPath {
			t.Fatalf("预期双方选择 %s 路径，发送方 %q，接收方 %q", wantPath, *senderPath, *receiverPath)
		}
		if !hub.received.Load() || hub.ackCalls.Load() != int32(index+2) {
			t.Fatal("中心应且只应收到一次对应投递的接收确认")
		}
	}
}

// pinRTCTransfer 为签名试点绑定本次投递和双方公钥，并验证拒绝 HTTPS 降级。
func pinRTCTransfer(t *testing.T, sender, receiver *collaboration, transfer string) {
	t.Helper()
	pubA, keyA, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB, keyB, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sender.rtc.pinned = rtcSigningScope{transfer: transfer, local: sender.identity.DeviceID, remote: receiver.identity.DeviceID, localKey: pubA, peerKey: pubB}
	sender.rtc.signingKey = keyA
	receiver.rtc.pinned = rtcSigningScope{transfer: transfer, local: receiver.identity.DeviceID, remote: sender.identity.DeviceID, localKey: pubB, peerKey: pubA}
	receiver.rtc.signingKey = keyB
	request := httptest.NewRequest(http.MethodPut, "/peer/v1/transfers/"+transfer, nil)
	request.SetPathValue("id", transfer)
	response := httptest.NewRecorder()
	receiver.receive(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("签名投递应拒绝传统 HTTPS 入口，得到 %d", response.Code)
	}
}

func checkRTCTransfer(t *testing.T, hub *testRTCHub, sender, receiver *collaboration, body []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	sent := make(chan error, 1)
	go func() { sent <- sender.push(ctx, hub.manifest, body) }()
	select {
	case offer := <-hub.offer:
		if err := receiver.rtc.receive(ctx, offer); err != nil {
			cancel()
			<-sent
			t.Fatal(err)
		}
	case err := <-sent:
		t.Fatalf("发送方在发出提议前结束，得到 %v", err)
	case <-ctx.Done():
		<-sent
		t.Fatal(ctx.Err())
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	var saved string
	var digest string
	if err := receiver.worker.db.QueryRowContext(ctx, "SELECT body,sha256 FROM peer_inbox WHERE id=?", hub.manifest.ID).Scan(&saved, &digest); err != nil || saved != string(body) || digest != transferHash(body) {
		t.Fatalf("收件箱正文或摘要不符，得到 %q、%q 和 %v", saved, digest, err)
	}
}
