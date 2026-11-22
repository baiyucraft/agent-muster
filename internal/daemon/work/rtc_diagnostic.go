package work

import (
	"context"
	"net/netip"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/peer"
	"github.com/pion/webrtc/v4"
)

// rtcDiagnosticReporter 是设备客户端的可选接口，未启用诊断的客户端仍能执行交接。
type rtcDiagnosticReporter interface {
	ReportRTCDiagnostic(context.Context, control.Identity, string, peer.RTCDiagnostic) error
}

// rtcDiagnostic 只采集 Hub 指定投递的握手状态。阶段由投递 goroutine 更新，
// 握手和网络采样各自持有独立快照，结束时经通道合并。
type rtcDiagnostic struct {
	upload     func(context.Context, peer.RTCDiagnostic) error
	attempt    string
	stage      string
	gateway    string
	sample     <-chan peer.RTCDiagnostic
	network    <-chan peer.RTCDiagnostic
	stopSample context.CancelFunc
}

// startDiagnostic 只在登记配置指定本次投递时启动采样，网络探测与握手并行。
func (transport *rtcTransport) startDiagnostic(ctx context.Context, connection *webrtc.PeerConnection, transfer, attempt, stage string) *rtcDiagnostic {
	transport.mu.RLock()
	enabled := transfer != "" && transport.diagnosticTransferID == transfer
	transport.mu.RUnlock()
	reporter, available := transport.hub.(rtcDiagnosticReporter)
	if !enabled || !available {
		return nil
	}

	sampleCtx, stopSample := context.WithCancel(ctx)
	sample := make(chan peer.RTCDiagnostic, 1)
	go func() {
		sample <- collectRTCHandshake(sampleCtx, connection)
	}()
	network := make(chan peer.RTCDiagnostic, 1)
	go func() {
		var report peer.RTCDiagnostic
		addRTCNetworkDiagnostic(ctx, &report)
		network <- report
	}()
	return &rtcDiagnostic{
		upload: func(ctx context.Context, report peer.RTCDiagnostic) error {
			return reporter.ReportRTCDiagnostic(ctx, transport.peer.identity, transfer, report)
		},
		attempt:    attempt,
		stage:      stage,
		sample:     sample,
		network:    network,
		stopSample: stopSample,
	}
}

// setGatewayMapping 记录网关映射试验的分类，不保存映射地址或端口。
func (diagnostic *rtcDiagnostic) setGatewayMapping(route string) {
	if diagnostic != nil {
		diagnostic.gateway = route
	}
}

// mark 保存已进入的阶段，打开数据通道后固定握手快照。
func (diagnostic *rtcDiagnostic) mark(stage string) {
	if diagnostic == nil {
		return
	}
	diagnostic.stage = stage
	if stage == "stream" {
		diagnostic.stopSample()
	}
}

// finish 在握手期限外尽力上报一次。失败时仍由原传输流程决定是否重试。
func (diagnostic *rtcDiagnostic) finish(ctx context.Context) {
	if diagnostic == nil {
		return
	}
	diagnostic.stopSample()
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var report peer.RTCDiagnostic
	select {
	case report = <-diagnostic.sample:
	case <-reportCtx.Done():
		return
	}
	// 若数据通道很快结束，辅助网络探测可能仍在运行。优先保存此次握手结果。
	select {
	case network := <-diagnostic.network:
		report.IPv6Global = network.IPv6Global
		report.IPv6DefaultRoute = network.IPv6DefaultRoute
		report.IPv4Mapping = network.IPv4Mapping
	default:
	}
	report.AttemptID = diagnostic.attempt
	report.Stage = diagnostic.stage
	report.GatewayMapping = diagnostic.gateway
	_ = diagnostic.upload(reportCtx, report)
}

// collectRTCHandshake 每 200 毫秒取一次快照，在 Pion 失败后清空候选对时保留已有计数。
func collectRTCHandshake(ctx context.Context, connection *webrtc.PeerConnection) peer.RTCDiagnostic {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var report peer.RTCDiagnostic
	for {
		finished := ctx.Err() != nil
		mergeRTCHandshakeStats(&report, connection.GetStats())
		dtls := connection.SCTP().Transport()
		report.ICEState = connection.ICEConnectionState().String()
		report.DTLSState = dtls.State().String()
		if pair, err := dtls.ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
			report.SelectedPair = rtcDiagnosticPair(pair)
		}
		if finished {
			return report
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

// mergeRTCHandshakeStats 只保留候选数量和候选对的计数峰值，不保存临时地址或候选对 ID。
func mergeRTCHandshakeStats(report *peer.RTCDiagnostic, stats webrtc.StatsReport) {
	var current peer.RTCDiagnostic
	for _, item := range stats {
		switch stat := item.(type) {
		case webrtc.ICECandidateStats:
			countRTCDiagnosticCandidate(&current, stat)
		case webrtc.ICECandidatePairStats:
			// Pion 会在写入套接字前增加请求计数，表示尝试次数。
			current.RequestsSent += stat.RequestsSent
			current.RequestsReceived += stat.RequestsReceived
			current.ResponsesSent += stat.ResponsesSent
			current.ResponsesReceived += stat.ResponsesReceived
		}
	}
	report.LocalIPv4Host = max(report.LocalIPv4Host, min(current.LocalIPv4Host, 32))
	report.LocalIPv4Reflexive = max(report.LocalIPv4Reflexive, min(current.LocalIPv4Reflexive, 32))
	report.LocalIPv6Host = max(report.LocalIPv6Host, min(current.LocalIPv6Host, 32))
	report.LocalIPv6Reflexive = max(report.LocalIPv6Reflexive, min(current.LocalIPv6Reflexive, 32))
	report.RequestsSent = max(report.RequestsSent, min(current.RequestsSent, 1_000_000))
	report.RequestsReceived = max(report.RequestsReceived, min(current.RequestsReceived, 1_000_000))
	report.ResponsesSent = max(report.ResponsesSent, min(current.ResponsesSent, 1_000_000))
	report.ResponsesReceived = max(report.ResponsesReceived, min(current.ResponsesReceived, 1_000_000))
}

// countRTCDiagnosticCandidate 按本机候选的地址族及类型累加计数。
func countRTCDiagnosticCandidate(report *peer.RTCDiagnostic, candidate webrtc.ICECandidateStats) {
	if candidate.Type != webrtc.StatsTypeLocalCandidate {
		return
	}
	address, err := netip.ParseAddr(candidate.IP)
	if err != nil {
		return
	}
	ipv6 := address.Unmap().Is6()
	switch candidate.CandidateType {
	case webrtc.ICECandidateTypeHost:
		if ipv6 {
			report.LocalIPv6Host++
		} else {
			report.LocalIPv4Host++
		}
	case webrtc.ICECandidateTypeSrflx, webrtc.ICECandidateTypePrflx:
		if ipv6 {
			report.LocalIPv6Reflexive++
		} else {
			report.LocalIPv4Reflexive++
		}
	}
}

// rtcDiagnosticPair 将选中的候选对归约为报告允许的传输类型。
func rtcDiagnosticPair(pair *webrtc.ICECandidatePair) string {
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		return "relay"
	}
	address, err := netip.ParseAddr(pair.Local.Address)
	if err != nil {
		return ""
	}
	if address.Unmap().Is6() {
		return "ipv6_direct"
	}
	return "ipv4_direct"
}
