package gitdelivery

import "regexp"

var (
	commitID = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
	testLog  = regexp.MustCompile(`^test-[0-9]+/output\.log$`)
)

// Verification 关联独立候选验证与实际检出的 Git 对象。日志引用相对于设备任务目录。
type Verification struct {
	BaseOID        string  `json:"base_oid,omitempty"`
	CandidateOID   string  `json:"candidate_oid,omitempty"`
	TestExitCode   *uint32 `json:"test_exit_code,omitempty"`
	TestLogRef     string  `json:"test_log_ref,omitempty"`
	TestStatus     string  `json:"test_status"`
	ReviewAgent    string  `json:"review_agent,omitempty"`
	ReviewStatus   string  `json:"review_status,omitempty"`
	RepairAttempts int     `json:"repair_attempts,omitempty"`
}

// Valid 校验验证证据及状态，允许记下未能运行或未能确认的验证尝试。
func (value Verification) Valid() bool {
	if value.RepairAttempts < 0 || value.RepairAttempts > 5 ||
		(value.ReviewAgent != "" && value.ReviewAgent != "dsh" && value.ReviewAgent != "codex") ||
		(value.ReviewStatus == "") != (value.ReviewAgent == "") {
		return false
	}
	switch value.ReviewStatus {
	case "", "unverified":
	case "passed", "changes_requested":
		if value.TestStatus != "passed" {
			return false
		}
	default:
		return false
	}
	if value.BaseOID != "" && !commitID.MatchString(value.BaseOID) ||
		value.CandidateOID != "" && (!commitID.MatchString(value.CandidateOID) || len(value.BaseOID) != len(value.CandidateOID) || value.BaseOID == value.CandidateOID) ||
		value.TestLogRef != "" && !testLog.MatchString(value.TestLogRef) {
		return false
	}
	if value.TestExitCode != nil && (value.CandidateOID == "" || value.TestLogRef == "") {
		return false
	}
	switch value.TestStatus {
	case "passed":
		return value.TestExitCode != nil && *value.TestExitCode == 0
	case "failed":
		return value.TestExitCode != nil && *value.TestExitCode != 0
	case "unverified":
		return true
	default:
		return false
	}
}
