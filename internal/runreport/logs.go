package runreport

import (
	"errors"
	"regexp"
	"time"
	"unicode/utf8"
)

const (
	MaxLogRequests      = 4
	MaxLogFiles         = 8
	MaxLogTextBytes     = 64 << 10
	MaxLogResponseBytes = 400 << 10

	LogAvailable = "available"
	LogMissing   = "missing"
	LogFailed    = "failed"
)

var (
	logRequestID        = regexp.MustCompile(`^log-[0-9a-f]{32}$`)
	logRunID            = regexp.MustCompile(`^run-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	logInvocation       = regexp.MustCompile(`^cli-[0-9]{1,10}$`)
	reviewLogInvocation = regexp.MustCompile(`^review-[0-9]{1,10}$`)
)

// LogRequest 标识 Hub 为指定运行发起的一次设备日志读取。
type LogRequest struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	Invocation string `json:"invocation,omitempty"` // 空值请求修复或普通执行日志。
}

// Validate 校验请求标识、运行标识和可选 Review 日志目录的字面格式。
func (request LogRequest) Validate() error {
	if !logRequestID.MatchString(request.ID) || !logRunID.MatchString(request.RunID) ||
		request.Invocation != "" && !ValidReviewLogInvocation(request.Invocation) {
		return errors.New("日志请求标识无效")
	}
	return nil
}

// LogResponse 携带本次读取的状态和设备返回的日志片段。
type LogResponse struct {
	Status string    `json:"status"`
	Files  []LogFile `json:"files,omitempty"`
}

// LogFile 对应一次 CLI 调用的一条输出流。Text 使用 UTF-8，截断时 Truncated 为 true。
type LogFile struct {
	Invocation string `json:"invocation"`
	Stream     string `json:"stream"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
	// UpdatedAt 是打开文件时观察到的设备本地修改时间。
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// IsLogInvocation 判断目录名是否匹配设备用于 CLI 原始日志的格式。
func IsLogInvocation(name string) bool {
	return logInvocation.MatchString(name) || ValidReviewLogInvocation(name)
}

// ValidReviewLogInvocation 判断名称是否指向独立 Review 的设备日志目录。
func ValidReviewLogInvocation(name string) bool {
	return reviewLogInvocation.MatchString(name)
}

// Validate 校验日志状态、输出流身份及 UTF-8 正文总长度。
func (response LogResponse) Validate() error {
	switch response.Status {
	case LogAvailable:
		if len(response.Files) == 0 || len(response.Files) > MaxLogFiles {
			return errors.New("日志片段数量无效")
		}
	case LogMissing, LogFailed:
		if len(response.Files) == 0 {
			return nil
		}
		return errors.New("日志状态与片段不匹配")
	default:
		return errors.New("日志读取状态无效")
	}
	seen := make(map[[2]string]bool, len(response.Files))
	var size int
	for _, file := range response.Files {
		key := [2]string{file.Invocation, file.Stream}
		size += len(file.Text)
		if !IsLogInvocation(file.Invocation) || (file.Stream != "stdout" && file.Stream != "stderr") ||
			seen[key] || size > MaxLogTextBytes || !utf8.ValidString(file.Text) {
			return errors.New("日志片段格式无效")
		}
		seen[key] = true
	}
	return nil
}
