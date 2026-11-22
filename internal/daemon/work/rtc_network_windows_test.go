package work

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/stun/v4"
)

func TestCompareRTCSTUNMappings(t *testing.T) {
	for _, test := range []struct {
		name       string
		secondPort int
		want       string
	}{
		{name: "consistent", secondPort: 40001, want: "consistent"},
		{name: "endpoint_dependent", secondPort: 40002, want: "endpoint_dependent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ports := [2]int{40001, test.secondPort}
			var destinations [2]netip.AddrPort
			sources := make(chan netip.AddrPort, len(destinations))
			responded := make(chan error, len(destinations))
			for index, port := range ports {
				server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = server.Close() })
				if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				destinations[index] = server.LocalAddr().(*net.UDPAddr).AddrPort()
				go func() { responded <- answerRTCSTUNTestProbe(server, sources, port) }()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			got := compareRTCSTUNMappings(ctx, destinations)
			for range destinations {
				if err := <-responded; err != nil {
					t.Fatal(err)
				}
			}
			if first, second := <-sources, <-sources; first != second {
				t.Fatal("expected both STUN requests to use the same source socket")
			}
			if got != test.want {
				t.Fatalf("mapping = %q, want %q", got, test.want)
			}
		})
	}
}

// answerRTCSTUNTestProbe 先返回另一事务的映射，再应答真实请求。
func answerRTCSTUNTestProbe(server *net.UDPConn, sources chan<- netip.AddrPort, port int) error {
	var packet [1024]byte
	length, remote, err := server.ReadFromUDPAddrPort(packet[:])
	if err != nil {
		return err
	}
	sources <- remote
	request := stun.New()
	if err := stun.Decode(packet[:length], request); err != nil {
		return err
	}
	ip := net.IPv4(192, 0, 2, 20)
	mapped := stun.XORMappedAddress{IP: ip, Port: 40099}
	spoof := stun.MustBuild(stun.TransactionID, stun.BindingSuccess, &mapped)
	if _, err := server.WriteToUDPAddrPort(spoof.Raw, remote); err != nil {
		return err
	}
	mapped.Port = port
	response := stun.MustBuild(request, stun.BindingSuccess, &mapped)
	_, err = server.WriteToUDPAddrPort(response.Raw, remote)
	return err
}
