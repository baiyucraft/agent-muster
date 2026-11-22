package work

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestRTCPortMappingPCP(t *testing.T) {
	for _, test := range []struct {
		status string
		addr   string
	}{
		{status: "global", addr: "1.2.3.4"},
		{status: "private", addr: "192.168.1.2"},
	} {
		t.Run(test.status, func(t *testing.T) {
			mappedAddress := netip.MustParseAddr(test.addr).As16()
			var nonce []byte
			gateway, done := rtcPortMapTestGateway(t, []func([]byte) []byte{
				func(packet []byte) []byte {
					if len(packet) != 60 || packet[0] != 2 || packet[1] != 1 || packet[36] != 17 ||
						binary.BigEndian.Uint16(packet[40:42]) != 43210 || binary.BigEndian.Uint32(packet[4:8]) == 0 ||
						netip.AddrFrom16([16]byte(packet[44:60])).Unmap() != netip.IPv4Unspecified() {
						t.Error("invalid PCP MAP request")
						return nil
					}
					nonce = append([]byte(nil), packet[24:36]...)
					response := make([]byte, 60)
					response[0], response[1] = 2, 0x81
					copy(response[4:8], packet[4:8])
					copy(response[24:42], packet[24:42])
					binary.BigEndian.PutUint16(response[42:44], 55001)
					copy(response[44:60], mappedAddress[:])
					return response
				},
				func(packet []byte) []byte {
					if len(packet) != 60 || packet[0] != 2 || packet[1] != 1 ||
						!bytes.Equal(nonce, packet[24:36]) || binary.BigEndian.Uint16(packet[40:42]) != 43210 ||
						binary.BigEndian.Uint32(packet[4:8]) != 0 || !bytes.Equal(make([]byte, 18), packet[42:60]) {
						t.Error("PCP delete did not match its lease")
						return nil
					}
					response := make([]byte, 60)
					response[0], response[1] = 2, 0x81
					copy(response[24:], packet[24:])
					return response
				},
			})
			mapped, status, release := rtcGatewayPortMap(context.Background(), netip.MustParseAddrPort("127.0.0.1:43210"), gateway)
			if test.status == "private" {
				<-done // 私网租约需要在返回前撤销。
			}
			release()
			release()
			<-done
			if status != test.status || mapped.IsValid() != (test.status == "global") || (mapped.IsValid() && mapped.Port() != 55001) {
				t.Fatalf("mapping = %v, classification = %s", mapped, status)
			}
		})
	}
}

func TestRTCPortMappingNATPMP(t *testing.T) {
	gateway, done := rtcPortMapTestGateway(t, []func([]byte) []byte{
		func([]byte) []byte { return append([]byte{2, 0x81, 0, 4}, make([]byte, 20)...) },
		func(packet []byte) []byte {
			if !bytes.Equal(packet, []byte{0, 0}) {
				t.Error("expected NAT-PMP external IPv4 request")
				return nil
			}
			return []byte{0, 0x80, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4}
		},
		func(packet []byte) []byte {
			if len(packet) != 12 || packet[0] != 0 || packet[1] != 1 || binary.BigEndian.Uint16(packet[4:6]) != 43210 || binary.BigEndian.Uint32(packet[8:12]) == 0 {
				t.Error("expected NAT-PMP UDP mapping for the ICE socket")
				return nil
			}
			return append([]byte{0, 0x81, 0, 0, 0, 0, 0, 0, 0xa8, 0xca, 0xd6, 0xd9}, packet[8:12]...)
		},
		func(packet []byte) []byte {
			if !bytes.Equal(packet, []byte{0, 1, 0, 0, 0xa8, 0xca, 0, 0, 0, 0, 0, 0}) {
				t.Error("expected zero external port and lifetime in NAT-PMP delete")
				return nil
			}
			return []byte{0, 0x81, 0, 0, 0, 0, 0, 0, 0xa8, 0xca, 0, 0, 0, 0, 0, 0}
		},
	})
	mapped, status, release := rtcGatewayPortMap(context.Background(), netip.MustParseAddrPort("127.0.0.1:43210"), gateway)
	release()
	<-done
	if status != "global" || mapped != netip.MustParseAddrPort("1.2.3.4:55001") {
		t.Fatalf("mapping = %v, classification = %s", mapped, status)
	}
}

// rtcPortMapTestGateway 在回环接口按顺序应答租约申请和撤销，测试不接触真实网关。
func rtcPortMapTestGateway(t *testing.T, responses []func([]byte) []byte) (netip.AddrPort, <-chan struct{}) {
	t.Helper()
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var datagram [1024]byte
		for _, respond := range responses {
			n, remote, err := server.ReadFromUDPAddrPort(datagram[:])
			if err != nil {
				t.Error(err)
				return
			}
			packet := respond(datagram[:n])
			if packet == nil {
				return
			}
			if _, err := server.WriteToUDPAddrPort(packet, remote); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	return server.LocalAddr().(*net.UDPAddr).AddrPort(), done
}
