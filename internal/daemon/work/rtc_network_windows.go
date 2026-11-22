package work

import (
	"context"
	"net"
	"net/netip"
	"time"
	"unsafe"

	"github.com/baiyucraft/agent-muster/internal/peer"
	"github.com/pion/stun/v4"
	"golang.org/x/sys/windows"
)

// addRTCNetworkDiagnostic 读取本机 IPv6 配置并限时比较 IPv4 UDP 映射。
// STUN 地址和映射值只用于本次比较，报告保留布尔值和分类。
func addRTCNetworkDiagnostic(ctx context.Context, report *peer.RTCDiagnostic) {
	report.IPv6Global = rtcGlobalIPv6()
	report.IPv6DefaultRoute = rtcIPv6DefaultRoute()
	report.IPv4Mapping = "inconclusive"
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	first := resolveRTCSTUN(ctx, "stun.l.google.com", 19302)
	second := resolveRTCSTUN(ctx, "stun.cloudflare.com", 3478)
	if first.IsValid() && second.IsValid() && first != second {
		report.IPv4Mapping = compareRTCSTUNMappings(ctx, [2]netip.AddrPort{first, second})
	}
}

// rtcGlobalIPv6 检查本机运行接口是否配置了公网 IPv6 地址。
func rtcGlobalIPv6() bool {
	interfaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	publicPrefix := netip.MustParsePrefix("2000::/3")
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			if ip, ok := address.(*net.IPNet); ok {
				addr, valid := netip.AddrFromSlice(ip.IP)
				if valid && publicPrefix.Contains(addr) {
					return true
				}
			}
		}
	}
	return false
}

// rtcIPv6DefaultRoute 查询运行接口上的 IPv6 默认路由。实际对端可达性由 ICE 验证。
func rtcIPv6DefaultRoute() bool {
	var table *windows.MibIpForwardTable2
	if windows.GetIpForwardTable2(windows.AF_INET6, &table) != nil || table == nil {
		return false
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	for _, route := range table.Rows() {
		if route.DestinationPrefix.PrefixLength != 0 || route.Loopback != 0 {
			continue
		}
		// IPv4 与 IPv6 接口索引可能不同，使用路由表提供的接口 LUID 查询状态。
		iface := windows.MibIfRow2{InterfaceLuid: route.InterfaceLuid}
		if windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &iface) == nil && iface.OperStatus == windows.IfOperStatusUp {
			return true
		}
	}
	return false
}

// resolveRTCSTUN 从预设的 STUN 主机解析一个公网 IPv4 目的地。
func resolveRTCSTUN(ctx context.Context, hostname string, port uint16) netip.AddrPort {
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", hostname)
	if err == nil {
		for _, address := range addresses {
			if address.Is4() && address.IsGlobalUnicast() && !address.IsPrivate() {
				return netip.AddrPortFrom(address, port)
			}
		}
	}
	return netip.AddrPort{}
}

// compareRTCSTUNMappings 从同一源地址和端口请求两个 STUN 目的地。
// 两处映射均已确认时才可比较，相同结果仅代表本次选定的两个目的地。
func compareRTCSTUNMappings(ctx context.Context, destinations [2]netip.AddrPort) string {
	deadline, ok := ctx.Deadline()
	if !ok {
		return "inconclusive"
	}
	// UDP connect 只用于读取路由选择的源地址，实际探测共用下方的未连接套接字。
	var dialer net.Dialer
	route, err := dialer.DialContext(ctx, "udp4", destinations[0].String())
	if err != nil {
		return "inconclusive"
	}
	source := route.LocalAddr().(*net.UDPAddr).IP
	_ = route.Close()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: source})
	if err != nil {
		return "inconclusive"
	}
	defer socket.Close()
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	if socket.SetDeadline(deadline) != nil {
		return "inconclusive"
	}

	var requests [2]*stun.Message
	for index, destination := range destinations {
		requests[index], err = stun.Build(stun.TransactionID, stun.BindingRequest)
		if err != nil {
			return "inconclusive"
		}
		if _, err := socket.WriteToUDPAddrPort(requests[index].Raw, destination); err != nil {
			return "inconclusive"
		}
	}
	var mappings [2]netip.AddrPort
	var packet [1024]byte
	response := stun.New()
	for !mappings[0].IsValid() || !mappings[1].IsValid() {
		length, remote, err := socket.ReadFromUDPAddrPort(packet[:])
		if err != nil {
			return "inconclusive"
		}
		for index, destination := range destinations {
			if remote == destination && !mappings[index].IsValid() {
				mappings[index] = decodeRTCSTUNMapping(packet[:length], requests[index], response)
				break
			}
		}
	}
	if mappings[0] != mappings[1] {
		return "endpoint_dependent"
	}
	return "consistent"
}

// decodeRTCSTUNMapping 只接受对应请求的 Binding Success 响应和 IPv4 XOR-MAPPED-ADDRESS。
func decodeRTCSTUNMapping(packet []byte, request, response *stun.Message) netip.AddrPort {
	if !stun.IsMessage(packet) || stun.Decode(packet, response) != nil || response.Type != stun.BindingSuccess ||
		response.TransactionID != request.TransactionID {
		return netip.AddrPort{}
	}
	var mapped stun.XORMappedAddress
	if mapped.GetFrom(response) != nil || mapped.Port <= 0 || mapped.Port > 65535 {
		return netip.AddrPort{}
	}
	address, ok := netip.AddrFromSlice(mapped.IP)
	if !ok || !address.Unmap().Is4() {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(address.Unmap(), uint16(mapped.Port))
}
