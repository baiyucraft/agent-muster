package work

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestMappedRTCConnectionAdvertisesBoundCandidate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	socket := listenMappedRTCLoopback(t)
	local := socket.LocalAddr().(*net.UDPAddr).AddrPort()
	mappedPort := uint16(41111)
	if mappedPort == local.Port() {
		mappedPort++
	}
	mapped := netip.AddrPortFrom(netip.MustParseAddr("203.0.113.41"), mappedPort)
	connection, closeConnection, err := newMappedRTCConnection("http://localhost", nil, socket, mapped)
	if err != nil {
		t.Fatal(err)
	}
	defer closeConnection()
	if _, err := connection.CreateDataChannel(rtcLabel, nil); err != nil {
		t.Fatal(err)
	}
	offer, err := connection.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	sdp, err := localDescription(ctx, connection, offer)
	if err != nil {
		t.Fatal(err)
	}
	expected := fmt.Sprintf(" %s %d typ host", mapped.Addr(), mapped.Port())
	if !strings.Contains(sdp, expected) {
		t.Fatalf("应发布映射候选 %s", mapped)
	}
	if strings.Contains(sdp, fmt.Sprintf(" %s %d typ host", local.Addr(), mapped.Port())) {
		t.Fatalf("不应将映射端口用于本机地址 %s", local.Addr())
	}
}

func TestMappedRTCConnectionOpensDataChannel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socket := listenMappedRTCLoopback(t)
	local := socket.LocalAddr().(*net.UDPAddr).AddrPort()
	sender, closeSender, err := newMappedRTCConnection("http://localhost", nil, socket, local)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSender()
	settings := rtcSettings("http://localhost")
	receiver, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sent := make(chan rtcStream, 1)
	received := make(chan rtcStream, 1)
	receiver.OnDataChannel(func(channel *webrtc.DataChannel) { publishRTCStream(channel, received) })
	channel, err := sender.CreateDataChannel(rtcLabel, nil)
	if err != nil {
		t.Fatal(err)
	}
	publishRTCStream(channel, sent)
	offer, err := sender.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	offer.SDP, err = localDescription(ctx, sender, offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := receiver.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	answer.SDP, err = localDescription(ctx, receiver, answer)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	out, err := waitRTCStream(ctx, sent)
	if err != nil {
		t.Fatal(err)
	}
	in, err := waitRTCStream(ctx, received)
	if err != nil {
		t.Fatal(err)
	}
	if err := rtcDeadline(out); err != nil {
		t.Fatal(err)
	}
	if err := rtcDeadline(in); err != nil {
		t.Fatal(err)
	}
	if err := writeRTCPacket(out, []byte("probe")); err != nil {
		t.Fatal(err)
	}
	if err := expectRTCPacket(in, "probe"); err != nil {
		t.Fatal(err)
	}
	pair, err := sender.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local.Port != local.Port() {
		t.Fatalf("数据通道应选择原 UDP socket 的端口，候选对 %v，错误 %v", pair, err)
	}
}

func listenMappedRTCLoopback(t *testing.T) *net.UDPConn {
	t.Helper()
	socket, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	return socket
}
