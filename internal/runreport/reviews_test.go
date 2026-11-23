package runreport

import (
	"encoding/json"
	"testing"
)

func TestDiagnosticsValidateReviewAttribution(t *testing.T) {
	report := Diagnostics{Reviews: []ReviewAttempt{
		{CandidateOID: stringOf('a', 40), RepairIndex: 0, AgentKind: "codex", Status: "changes_requested",
			Diagnostics: &Diagnostics{ErrorKind: "native_exit", ExitCode: uint32Pointer(1)}, LogInvocation: "review-1"},
		{CandidateOID: stringOf('b', 40), RepairIndex: 1, AgentKind: "dsh", Status: "passed", LogInvocation: "review-2"},
	}}
	if !report.Valid() {
		t.Fatal("有效 Review 诊断未通过校验")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Diagnostics
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded.Valid() || len(decoded.Reviews) != 2 {
		t.Fatalf("Review 诊断不能往返：%s，%v", encoded, err)
	}

	invalid := report
	invalid.Reviews = append([]ReviewAttempt(nil), report.Reviews...)
	invalid.Reviews[1].RepairIndex = 0
	if invalid.Valid() {
		t.Fatal("重复返修序号应无效")
	}
	invalid = report
	invalid.Reviews = append([]ReviewAttempt(nil), report.Reviews...)
	invalid.Reviews[0].Diagnostics = &Diagnostics{Reviews: []ReviewAttempt{{}}}
	if invalid.Valid() {
		t.Fatal("嵌套 Review 诊断应无效")
	}
	invalid.Reviews[0].Diagnostics = &Diagnostics{Usage: &Usage{Source: DSHHeadless}}
	if invalid.Valid() {
		t.Fatal("Codex Review 不应携带 dsh 用量")
	}
}

func TestLogRequestCanSelectReviewInvocation(t *testing.T) {
	request := LogRequest{ID: "log-0123456789abcdef0123456789abcdef", RunID: "run-01234567-89ab-cdef-0123-456789abcdef", Invocation: "review-9"}
	if err := request.Validate(); err != nil {
		t.Fatalf("精确 Review 日志请求应有效：%v", err)
	}
	request.Invocation = "review-x"
	if err := request.Validate(); err == nil {
		t.Fatal("格式错误的 Review 调用引用应无效")
	}
}

func stringOf(value byte, length int) string {
	result := make([]byte, length)
	for i := range result {
		result[i] = value
	}
	return string(result)
}

func uint32Pointer(value uint32) *uint32 { return &value }
