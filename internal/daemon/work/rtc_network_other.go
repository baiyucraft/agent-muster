//go:build !windows

package work

import (
	"context"

	"github.com/baiyucraft/agent-muster/internal/peer"
)

// addRTCNetworkDiagnostic 的网络配置探测目前由 Windows 实现。
func addRTCNetworkDiagnostic(_ context.Context, report *peer.RTCDiagnostic) {
	report.IPv4Mapping = "inconclusive"
}
