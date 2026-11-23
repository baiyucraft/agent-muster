// Package runreport 定义设备与 Hub 共用的运行诊断契约。
package runreport

import (
	"encoding/json"
	"errors"
	"regexp"
)

const (
	// DSHHeadless 表示 dsh 当前会话的 step_end 样本。输入计数不含缓存读写。
	DSHHeadless = "dsh_headless"
	// CodexExec 表示 Codex 新线程的 turn.completed 样本。缓存输入是输入计数的子集。
	CodexExec = "codex_exec"
	// DSHCenterTurn 表示中心插件从完整原生轮次确认的用量，输入计数不含缓存读写。
	DSHCenterTurn = "dsh_center_turn"
)

// Usage 保存 CLI 提供的 Token 计数。缺失的可选字段保持未知。
type Usage struct {
	Source                string `json:"source"`
	InputTokens           int64  `json:"input_tokens"`
	OutputTokens          int64  `json:"output_tokens"`
	CachedInputTokens     *int64 `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens *int64 `json:"cache_write_input_tokens,omitempty"`
	ReasoningOutputTokens *int64 `json:"reasoning_output_tokens,omitempty"`
	TotalTokens           *int64 `json:"total_tokens,omitempty"`
}

// UnmarshalJSON 要求 CLI 给出输入和输出计数，显式零值有效。
func (usage *Usage) UnmarshalJSON(data []byte) error {
	type fields Usage
	value := struct {
		*fields
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	}{fields: (*fields)(usage)}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value.InputTokens == nil || value.OutputTokens == nil {
		return errors.New("运行用量缺少输入或输出计数")
	}
	usage.InputTokens, usage.OutputTokens = *value.InputTokens, *value.OutputTokens
	return nil
}

// Diagnostics 记录运行结束时设备能确认的退出码、已落盘字节数和用量。
// 缺失字段表示未知。日志正文和本机路径保存在设备。
type Diagnostics struct {
	StdoutBytes       *int64          `json:"stdout_bytes,omitempty"`
	StderrBytes       *int64          `json:"stderr_bytes,omitempty"`
	ExitCode          *uint32         `json:"exit_code,omitempty"`
	CaptureIncomplete bool            `json:"capture_incomplete,omitempty"`
	ErrorKind         string          `json:"error_kind,omitempty"`
	Usage             *Usage          `json:"usage,omitempty"`
	Reviews           []ReviewAttempt `json:"reviews,omitempty"`
}

var reviewCandidateOID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ReviewAttempt 记录独立审查对指定候选提交的一次验证。RepairIndex 从零开始，
// 对应产生候选的返修次数。诊断及日志引用仅归属本次审查调用。
type ReviewAttempt struct {
	CandidateOID  string       `json:"candidate_oid"`
	RepairIndex   int          `json:"repair_index"`
	AgentKind     string       `json:"agent_kind"`
	Status        string       `json:"status"`
	Diagnostics   *Diagnostics `json:"diagnostics,omitempty"`
	LogInvocation string       `json:"log_invocation,omitempty"`
}

// Valid 校验候选、返修序号、审查状态及一次 CLI 调用的诊断范围。
func (attempt ReviewAttempt) Valid() bool {
	switch attempt.Status {
	case "passed", "changes_requested", "unverified":
	default:
		return false
	}
	var source string
	switch attempt.AgentKind {
	case "codex":
		source = CodexExec
	case "dsh":
		source = DSHHeadless
	default:
		return false
	}
	diagnostics := attempt.Diagnostics
	if diagnostics != nil && (len(diagnostics.Reviews) != 0 || !diagnostics.Valid() ||
		diagnostics.Usage != nil && diagnostics.Usage.Source != source) {
		return false
	}
	return reviewCandidateOID.MatchString(attempt.CandidateOID) &&
		attempt.RepairIndex >= 0 && attempt.RepairIndex <= 5 &&
		(attempt.LogInvocation == "" || ValidReviewLogInvocation(attempt.LogInvocation))
}

// Valid 校验诊断枚举和浏览器可精确呈现的非负整数范围。
func (report *Diagnostics) Valid() bool {
	if report == nil {
		return true
	}
	switch report.ErrorKind {
	case "", "native_exit", "interrupted", "output", "setup":
	default:
		return false
	}
	if !validQuantity(report.StdoutBytes) || !validQuantity(report.StderrBytes) {
		return false
	}
	lastRepair := -1
	seenLogs := make(map[string]bool, len(report.Reviews))
	for _, review := range report.Reviews {
		if !review.Valid() || review.RepairIndex <= lastRepair || review.LogInvocation != "" && seenLogs[review.LogInvocation] {
			return false
		}
		lastRepair = review.RepairIndex
		seenLogs[review.LogInvocation] = true
	}
	u := report.Usage
	if u == nil {
		return true
	}
	return (u.Source == DSHHeadless || u.Source == CodexExec) && u.validCounts()
}

// ValidCenterUsage 校验中心轮次用量。空值表示没有可确认的样本。
func (usage *Usage) ValidCenterUsage() bool {
	return usage == nil || usage.Source == DSHCenterTurn && usage.TotalTokens != nil && usage.validCounts()
}

func (usage *Usage) validCounts() bool {
	return validQuantity(&usage.InputTokens) && validQuantity(&usage.OutputTokens) &&
		validQuantity(usage.CachedInputTokens) && validQuantity(usage.CacheWriteInputTokens) &&
		validQuantity(usage.ReasoningOutputTokens) && validQuantity(usage.TotalTokens)
}

func validQuantity(value *int64) bool {
	const maxExact = 1<<53 - 1
	return value == nil || *value >= 0 && *value <= maxExact
}
