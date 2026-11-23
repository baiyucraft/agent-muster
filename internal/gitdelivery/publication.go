package gitdelivery

import "github.com/baiyucraft/agent-muster/internal/inputreceipt"

// PushRequest 将设备当前轮次、已核验提交和本机解析的远端指纹绑定到同一推送意图。
type PushRequest struct {
	Generation             int64                 `json:"generation"`
	Lease                  string                `json:"lease"`
	Verification           Verification          `json:"verification"`
	DestinationFingerprint string                `json:"destination_fingerprint"`
	InputReceipt           *inputreceipt.Receipt `json:"input_receipt,omitempty"` // 推送前确认本轮输入已处理。
}

// Publication 是管理员可见的推送和回读记录。Remote 为仓库远端别名。
type Publication struct {
	ID           string `json:"id"`
	Revision     int64  `json:"revision"`
	Remote       string `json:"remote"`
	TargetRef    string `json:"target_ref"`
	CandidateOID string `json:"candidate_oid"`
	Status       string `json:"status"`
	ObservedOID  string `json:"observed_oid,omitempty"`
}

// PushIntent 是设备收到的固定推送目标和本次操作许可。
type PushIntent struct {
	Publication
	DestinationFingerprint string `json:"destination_fingerprint"`
	PushAllowed            bool   `json:"push_allowed"`
}

// Observation 记录对该意图指定的远端引用执行的读回。
type Observation struct {
	Generation  int64  `json:"generation"`
	Lease       string `json:"lease"`
	Status      string `json:"status"`
	ObservedOID string `json:"observed_oid,omitempty"`
}
