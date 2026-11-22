package work

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/peer"
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

type testSignedRTCHub struct {
	rtcHub
	testTransferHub
	offer  chan peer.RTCOffer
	answer chan peer.RTCAnswer
}

func (hub *testSignedRTCHub) PutRTCOffer(_ context.Context, _ control.Identity, transfer string, signal peer.RTCSignal) error {
	hub.offer <- peer.RTCOffer{TransferID: transfer, RTCSignal: signal}
	return nil
}

func (hub *testSignedRTCHub) PutRTCAnswer(_ context.Context, _ control.Identity, _ string, signal peer.RTCSignal) error {
	hub.answer <- peer.RTCAnswer{SDP: signal.SDP}
	return nil
}

func (hub *testSignedRTCHub) RTCAnswer(ctx context.Context, _ control.Identity, _, _ string) (peer.RTCAnswer, error) {
	select {
	case answer := <-hub.answer:
		return answer, nil
	case <-ctx.Done():
		return peer.RTCAnswer{}, ctx.Err()
	}
}

func TestRTCSignedTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
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
	)
	body := []byte("synthetic transfer")
	hub := &testSignedRTCHub{offer: make(chan peer.RTCOffer, 1), answer: make(chan peer.RTCAnswer, 1)}
	hub.manifest = control.Transfer{
		ID: transfer, SourceDeviceID: deviceA, TargetDeviceID: deviceB,
		Name: "fixture", Kind: "message", Size: int64(len(body)), SHA256: transferHash(body), Ticket: strings.Repeat("a", 64),
		Peer: &control.Peer{DeviceID: deviceB, Address: "not-used"},
	}
	hub.ackCalls.Store(1)
	open := func(local, remote string, key ed25519.PrivateKey, peerKey ed25519.PublicKey) *collaboration {
		t.Helper()
		worker, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = worker.Close() })
		device, err := openPeer(ctx, worker, hub, control.Identity{DeviceID: local, HubURL: "http://127.0.0.1"}, PeerConfig{})
		if err != nil {
			t.Fatal(err)
		}
		device.rtc.live = true
		device.rtc.pinned = rtcSigningScope{transfer: transfer, local: local, remote: remote, localKey: key.Public().(ed25519.PublicKey), peerKey: peerKey}
		device.rtc.signingKey = key
		return device
	}
	sender := open(deviceA, deviceB, keyA, pubB)
	receiver := open(deviceB, deviceA, keyB, pubA)
	request := httptest.NewRequest(http.MethodPut, "/peer/v1/transfers/"+transfer, nil)
	request.SetPathValue("id", transfer)
	response := httptest.NewRecorder()
	receiver.receive(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("签名投递应拒绝传统 HTTPS 入口，得到 %d", response.Code)
	}
	sent := make(chan error, 1)
	go func() { sent <- sender.push(ctx, hub.manifest, body) }()
	select {
	case offer := <-hub.offer:
		if err := receiver.rtc.receive(ctx, offer); err != nil {
			t.Fatal(err)
		}
	case err := <-sent:
		t.Fatalf("发送方在发出提议前结束，得到 %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := receiver.worker.db.QueryRowContext(ctx, "SELECT body FROM peer_inbox WHERE id=?", transfer).Scan(&saved); err != nil || saved != string(body) {
		t.Fatalf("应通过已签名的信令建立连接并确认收件，得到 %q, %v", saved, err)
	}
}
