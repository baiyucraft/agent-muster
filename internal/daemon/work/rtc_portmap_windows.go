package work

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	rtcPortMapLeaseSeconds = 180
	rtcPortMapMinimumLease = rtcOfferTimeout + rtcBodyTimeout + 5*time.Second
	rtcPortMapWait         = time.Second
)

var rtcGetBestRoute = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetBestRoute2")

// rtcDefaultIPv4Source 返回访问公网 IPv4 时，Windows 选定的默认路由源地址。
func rtcDefaultIPv4Source() (netip.Addr, bool) {
	source, _, ok := rtcDefaultIPv4Route()
	return source, ok
}

// rtcDefaultIPv4Route 查询 Windows 的公网 IPv4 路由和源地址，不发送网络请求。
func rtcDefaultIPv4Route() (source, gateway netip.Addr, ok bool) {
	if rtcGetBestRoute.Find() != nil {
		return source, gateway, false
	}
	var destination windows.RawSockaddrInet
	*(*windows.RawSockaddrInet4)(unsafe.Pointer(&destination)) = windows.RawSockaddrInet4{Family: windows.AF_INET, Addr: [4]byte{1, 1, 1, 1}}
	var route windows.MibIpForwardRow2
	var sourceAddress windows.RawSockaddrInet
	code, _, _ := rtcGetBestRoute.Call(0, 0, 0, uintptr(unsafe.Pointer(&destination)), 0,
		uintptr(unsafe.Pointer(&route)), uintptr(unsafe.Pointer(&sourceAddress)))
	if code != 0 || route.DestinationPrefix.PrefixLength != 0 || route.Loopback != 0 ||
		sourceAddress.Family != windows.AF_INET {
		return source, gateway, false
	}
	source = netip.AddrFrom4((*windows.RawSockaddrInet4)(unsafe.Pointer(&sourceAddress)).Addr)
	if route.NextHop.Family == windows.AF_INET {
		gateway = netip.AddrFrom4((*windows.RawSockaddrInet4)(unsafe.Pointer(&route.NextHop)).Addr)
	}
	return source, gateway, source.IsGlobalUnicast()
}

// acquireRTCPortMapping 为已经绑定默认 IPv4 源地址的 ICE 套接字申请临时 UDP 映射。
// 只有公网映射可以作为候选。release 可重复调用，并在独立期限内撤销本次租约。
func acquireRTCPortMapping(ctx context.Context, socket *net.UDPConn) (mapped netip.AddrPort, route string, release func()) {
	noop := func() {}
	source, gateway, ok := rtcDefaultIPv4Route()
	local := socket.LocalAddr().(*net.UDPAddr).AddrPort()
	if !ok || !gateway.IsGlobalUnicast() || local.Addr().Unmap() != source || local.Port() == 0 {
		return mapped, "unsupported", noop
	}
	mapped, route, release = rtcGatewayPortMap(ctx, local, netip.AddrPortFrom(gateway, 5351))
	if route != "unsupported" && route != "timeout" {
		return mapped, route, release
	}
	if upnpMapped, upnpRoute, upnpRelease := rtcUPnPPortMap(ctx, local, gateway); upnpRoute != "" {
		return upnpMapped, upnpRoute, upnpRelease
	}
	return mapped, route, release
}

// rtcGatewayPortMap 按 PCP、NAT-PMP 顺序请求指定网关。同一连接仅保留一条租约。
func rtcGatewayPortMap(ctx context.Context, local, gateway netip.AddrPort) (netip.AddrPort, string, func()) {
	noop := func() {}
	request := rtcPCPRequest(local)
	response, status := rtcPortMapExchange(ctx, local.Addr(), gateway, request[:], func(packet []byte) bool {
		return rtcValidPCPResponse(packet, request[:])
	})
	if status == "" && response[3] == 0 {
		mapped := netip.AddrPortFrom(netip.AddrFrom16([16]byte(response[44:60])).Unmap(), binary.BigEndian.Uint16(response[42:44]))
		lifetime := time.Duration(binary.BigEndian.Uint32(response[4:8])) * time.Second
		if lifetime > 0 {
			release := rtcPortMapCleanup(local.Addr(), gateway, rtcPCPDelete(request))
			if !rtcPublicMappedIPv4(mapped.Addr()) {
				release()
				return netip.AddrPort{}, "private", noop
			}
			if rtcPortMapLeaseAllows(lifetime) && mapped.Port() != 0 {
				return mapped, "global", release
			}
			release()
			return netip.AddrPort{}, "unsupported", noop
		}
	}
	if status == "timeout" {
		// 确认丢失时网关可能已创建 PCP 租约，使用原 nonce 尝试撤销。
		rtcPortMapCleanup(local.Addr(), gateway, rtcPCPDelete(request))()
	}

	response, status = rtcPortMapExchange(ctx, local.Addr(), gateway, []byte{0, 0}, rtcValidPMPAddress)
	if status != "" {
		return netip.AddrPort{}, status, noop
	}
	if binary.BigEndian.Uint16(response[2:4]) != 0 {
		return netip.AddrPort{}, "unsupported", noop
	}
	address := netip.AddrFrom4([4]byte(response[8:12]))
	if !rtcPublicMappedIPv4(address) {
		return netip.AddrPort{}, "private", noop
	}
	pmp := make([]byte, 12)
	pmp[1] = 1 // NAT-PMP UDP 映射。
	binary.BigEndian.PutUint16(pmp[4:6], local.Port())
	binary.BigEndian.PutUint32(pmp[8:12], rtcPortMapLeaseSeconds)
	response, status = rtcPortMapExchange(ctx, local.Addr(), gateway, pmp, func(packet []byte) bool {
		return rtcValidPMPMapping(packet, local.Port())
	})
	// NAT-PMP 用源地址、协议和内部端口定位租约，删除请求的外部端口和生存期为零。
	clear(pmp[8:12])
	release := rtcPortMapCleanup(local.Addr(), gateway, pmp)
	if status != "" {
		if status == "timeout" {
			release()
		}
		return netip.AddrPort{}, status, noop
	}
	if binary.BigEndian.Uint16(response[2:4]) != 0 {
		return netip.AddrPort{}, "unsupported", noop
	}
	port := binary.BigEndian.Uint16(response[10:12])
	if !rtcPortMapLeaseAllows(time.Duration(binary.BigEndian.Uint32(response[12:16]))*time.Second) || port == 0 {
		release()
		return netip.AddrPort{}, "unsupported", noop
	}
	return netip.AddrPortFrom(address, port), "global", release
}

// rtcPCPRequest 按 RFC 6887 编码 MAP，随机 nonce 标识本次 UDP 租约。
func rtcPCPRequest(local netip.AddrPort) [60]byte {
	var request [60]byte
	request[0], request[1], request[36] = 2, 1, 17
	binary.BigEndian.PutUint32(request[4:8], rtcPortMapLeaseSeconds)
	client := local.Addr().As16()
	copy(request[8:24], client[:])
	_, _ = rand.Read(request[24:36])
	binary.BigEndian.PutUint16(request[40:42], local.Port())
	// 使用 IPv4 映射的未指定地址，明确向双栈 PCP 网关请求 IPv4 租约。
	request[54], request[55] = 0xff, 0xff
	return request
}

// rtcPCPDelete 保留租约标识，按 RFC 6887 §15.1 清空生存期与建议外部地址和端口。
func rtcPCPDelete(request [60]byte) []byte {
	clear(request[4:8])
	clear(request[42:60])
	return request[:]
}

// rtcPortMapLeaseAllows 仅接受可覆盖连接期限且不超过请求时长的租约。
func rtcPortMapLeaseAllows(lifetime time.Duration) bool {
	return lifetime >= rtcPortMapMinimumLease && lifetime <= rtcPortMapLeaseSeconds*time.Second
}

// rtcValidPCPResponse 核对 PCP MAP 的版本、操作和租约标识。短错误响应只用于降级。
func rtcValidPCPResponse(packet, request []byte) bool {
	if len(packet) < 24 || packet[0] != 2 || packet[1] != 0x81 {
		return false
	}
	if packet[3] != 0 && len(packet) == 24 {
		return true
	}
	return len(packet) >= 60 && bytes.Equal(packet[24:36], request[24:36]) && packet[36] == request[36] &&
		binary.BigEndian.Uint16(packet[40:42]) == binary.BigEndian.Uint16(request[40:42])
}

// rtcValidPMPAddress 识别只读的 NAT-PMP 外部地址响应。
func rtcValidPMPAddress(packet []byte) bool {
	return len(packet) == 12 && packet[0] == 0 && packet[1] == 0x80
}

// rtcValidPMPMapping 将 NAT-PMP UDP 响应与本次请求的内部端口关联。
func rtcValidPMPMapping(packet []byte, port uint16) bool {
	return len(packet) == 16 && packet[0] == 0 && packet[1] == 0x81 && binary.BigEndian.Uint16(packet[8:10]) == port
}

// rtcPortMapCleanup 返回只运行一次的撤销函数，连接结束后的撤销使用独立期限。
func rtcPortMapCleanup(source netip.Addr, gateway netip.AddrPort, request []byte) func() {
	return sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.Background(), rtcPortMapWait)
		defer cancel()
		rtcPortMapExchange(ctx, source, gateway, request, func(packet []byte) bool {
			if request[0] == 2 {
				return rtcValidPCPResponse(packet, request)
			}
			return rtcValidPMPMapping(packet, binary.BigEndian.Uint16(request[4:6]))
		})
	})
}

// rtcPortMapExchange 从映射所有者的源地址请求网关，只接收该网关的响应。
func rtcPortMapExchange(ctx context.Context, source netip.Addr, gateway netip.AddrPort, request []byte, valid func([]byte) bool) ([]byte, string) {
	ctx, cancel := context.WithTimeout(ctx, rtcPortMapWait)
	defer cancel()
	dialer := net.Dialer{LocalAddr: net.UDPAddrFromAddrPort(netip.AddrPortFrom(source, 0))}
	conn, err := dialer.DialContext(ctx, "udp4", gateway.String())
	if err != nil {
		return nil, "timeout"
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if conn.SetDeadline(deadline) != nil {
		return nil, "timeout"
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err := conn.Write(request); err != nil {
		return nil, "timeout"
	}
	var packet [1100]byte
	for {
		n, err := conn.Read(packet[:])
		if err != nil {
			return nil, "timeout"
		}
		if valid(packet[:n]) {
			return packet[:n], ""
		}
	}
}

// rtcPublicMappedIPv4 排除内网、共享地址及不能作为公网候选的特殊地址范围。
func rtcPublicMappedIPv4(addr netip.Addr) bool {
	if !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, block := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"} {
		if netip.MustParsePrefix(block).Contains(addr) {
			return false
		}
	}
	return true
}
