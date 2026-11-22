//go:build !windows

package work

import (
	"context"
	"net"
	"net/netip"
)

// rtcDefaultIPv4Source 在未实现系统路由查询的平台报告不可用。
func rtcDefaultIPv4Source() (netip.Addr, bool) {
	return netip.Addr{}, false
}

// acquireRTCPortMapping 在未实现网关探测的平台报告不支持。
func acquireRTCPortMapping(context.Context, *net.UDPConn) (netip.AddrPort, string, func()) {
	return netip.AddrPort{}, "unsupported", func() {}
}
