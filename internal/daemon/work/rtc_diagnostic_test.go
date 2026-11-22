package work

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/peer"
	"github.com/pion/webrtc/v4"
)

func TestRTCPortMapScopeRequiresSignedTransferAndUnexpiredDeadline(t *testing.T) {
	id := "transfer-12345678-1234-1234-1234-123456789abc"
	deadline := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	scope := id + "," + deadline.Format(time.RFC3339)
	if !validRTCPortMapScope(scope, id, deadline.Add(-time.Second)) {
		t.Fatal("signed transfer should be allowed before expiry")
	}
	if validRTCPortMapScope(scope, id, deadline) || validRTCPortMapScope("", id, deadline.Add(-time.Second)) ||
		validRTCPortMapScope(scope, id+"0", deadline.Add(-time.Second)) {
		t.Fatal("expired, absent and unmatched scope should be denied")
	}
}

func TestRTCDiagnosticPreservesStatsAfterICEFailure(t *testing.T) {
	var report peer.RTCDiagnostic
	mergeRTCHandshakeStats(&report, webrtc.StatsReport{
		"host4": webrtc.ICECandidateStats{
			Type: webrtc.StatsTypeLocalCandidate, CandidateType: webrtc.ICECandidateTypeHost, IP: "::ffff:192.0.2.1",
		},
		"reflexive6": webrtc.ICECandidateStats{
			Type: webrtc.StatsTypeLocalCandidate, CandidateType: webrtc.ICECandidateTypeSrflx, IP: "2001:db8::1",
		},
		"remote": webrtc.ICECandidateStats{
			Type: webrtc.StatsTypeRemoteCandidate, CandidateType: webrtc.ICECandidateTypeHost, IP: "192.0.2.2",
		},
		"pair_a": webrtc.ICECandidatePairStats{
			RequestsSent: 7, RequestsReceived: 2, ResponsesSent: 1,
		},
		"pair_b": webrtc.ICECandidatePairStats{
			RequestsSent: 4, ResponsesReceived: 1,
		},
	})
	mergeRTCHandshakeStats(&report, webrtc.StatsReport{
		"pair_b": webrtc.ICECandidatePairStats{RequestsSent: 5, ResponsesReceived: 2},
	})
	mergeRTCHandshakeStats(&report, nil)
	if report.LocalIPv4Host != 1 || report.LocalIPv6Reflexive != 1 {
		t.Fatalf("诊断应保留本机候选数量，得到 %+v", report)
	}
	if report.RequestsSent != 11 || report.RequestsReceived != 2 || report.ResponsesSent != 1 || report.ResponsesReceived != 2 {
		t.Fatalf("诊断应保留各项计数峰值，得到 %+v", report)
	}
}

func TestRTCDiagnosticReportsAfterHandshakeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sample := make(chan peer.RTCDiagnostic, 1)
	sample <- peer.RTCDiagnostic{ICEState: "failed", DTLSState: "new", RequestsSent: 11}
	network := make(chan peer.RTCDiagnostic, 1)
	network <- peer.RTCDiagnostic{IPv6Global: true, IPv6DefaultRoute: true, IPv4Mapping: "endpoint_dependent"}
	var report peer.RTCDiagnostic
	diagnostic := &rtcDiagnostic{
		upload: func(ctx context.Context, result peer.RTCDiagnostic) error {
			if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
				t.Fatalf("上报应有独立期限，得到 %v", ctx.Err())
			}
			report = result
			return errors.New("模拟 Hub 暂不可用")
		},
		attempt:    "abcdef0123456789abcdef0123456789",
		stage:      "wait_stream",
		sample:     sample,
		network:    network,
		stopSample: cancel,
	}
	diagnostic.finish(ctx)
	if !report.Valid() || report.AttemptID != diagnostic.attempt || report.Stage != "wait_stream" ||
		report.ICEState != "failed" || report.RequestsSent != 11 ||
		!report.IPv6Global || !report.IPv6DefaultRoute || report.IPv4Mapping != "endpoint_dependent" {
		t.Fatalf("应合并握手与网络快照，得到 %+v", report)
	}
}
