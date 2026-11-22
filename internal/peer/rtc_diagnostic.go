package peer

import "slices"

// RTCDiagnostic 记录一次连接尝试的阶段、候选类型和检查计数。
// 字段只接受固定枚举、布尔或计数，供 Hub 核对测试投递。
type RTCDiagnostic struct {
	AttemptID          string `json:"attempt_id"`
	Stage              string `json:"stage"`
	ICEState           string `json:"ice_state,omitempty"`
	DTLSState          string `json:"dtls_state,omitempty"`
	IPv6Global         bool   `json:"ipv6_global"`
	IPv6DefaultRoute   bool   `json:"ipv6_default_route"`
	IPv4Mapping        string `json:"ipv4_mapping,omitempty"`
	GatewayMapping     string `json:"gateway_mapping,omitempty"`
	LocalIPv4Host      int    `json:"local_ipv4_host"`
	LocalIPv4Reflexive int    `json:"local_ipv4_reflexive"`
	LocalIPv6Host      int    `json:"local_ipv6_host"`
	LocalIPv6Reflexive int    `json:"local_ipv6_reflexive"`
	RequestsSent       uint64 `json:"requests_sent"`
	RequestsReceived   uint64 `json:"requests_received"`
	ResponsesSent      uint64 `json:"responses_sent"`
	ResponsesReceived  uint64 `json:"responses_received"`
	SelectedPair       string `json:"selected_pair,omitempty"`
}

// Valid 检查诊断数据的值域，确保日志只接收限定的枚举和整数。
func (report RTCDiagnostic) Valid() bool {
	return ValidAttempt(report.AttemptID) &&
		slices.Contains([]string{"wait_answer", "apply_answer", "answer", "wait_stream", "stream", "complete"}, report.Stage) &&
		slices.Contains([]string{"", "new", "checking", "connected", "completed", "disconnected", "failed", "closed"}, report.ICEState) &&
		slices.Contains([]string{"", "new", "connecting", "connected", "failed", "closed"}, report.DTLSState) &&
		slices.Contains([]string{"", "endpoint_dependent", "consistent", "inconclusive"}, report.IPv4Mapping) &&
		slices.Contains([]string{"", "global", "private", "unsupported", "timeout", "unavailable"}, report.GatewayMapping) &&
		slices.Contains([]string{"", "ipv4_direct", "ipv6_direct", "relay"}, report.SelectedPair) &&
		min(report.LocalIPv4Host, report.LocalIPv4Reflexive, report.LocalIPv6Host, report.LocalIPv6Reflexive) >= 0 &&
		max(report.LocalIPv4Host, report.LocalIPv4Reflexive, report.LocalIPv6Host, report.LocalIPv6Reflexive) <= 32 &&
		max(report.RequestsSent, report.RequestsReceived, report.ResponsesSent, report.ResponsesReceived) <= 1_000_000
}
