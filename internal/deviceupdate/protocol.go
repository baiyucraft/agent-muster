// Package deviceupdate 定义 Hub 与设备端共享的软件更新协议。
package deviceupdate

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// StatusIdle 表示设备当前没有更新任务。
	StatusIdle = "idle"
	// StatusPending 表示 Hub 已保存目标，正在等待设备处理。
	StatusPending = "pending"
	// StatusDownloading 表示设备正在下载或校验发布制品。
	StatusDownloading = "downloading"
	// StatusWaiting 表示设备已准备好制品，正在等待执行器空闲。
	StatusWaiting = "waiting"
	// StatusApplying 表示 Hub 已确认切换请求，设备可以开始安装。
	StatusApplying = "applying"
	// StatusSucceeded 表示设备已报告运行目标版本。
	StatusSucceeded = "succeeded"
	// StatusFailed 表示该次更新失败。
	StatusFailed = "failed"
	// StatusRolledBack 表示设备切换失败后已恢复旧版本。
	StatusRolledBack = "rolled_back"
)

var (
	// ErrConflict 表示下发操作与设备当前版本或更新状态冲突。
	ErrConflict = errors.New("device update conflicts with current state")

	versionPattern    = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)
	assignmentPattern = regexp.MustCompile(`^upd-[a-f0-9]{32}$`)
)

// Target 标识一次更新任务及其目标版本。ID 用于关联进度和安装确认。
type Target struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	ManifestURL string    `json:"manifest_url"`
	AssignedAt  time.Time `json:"assigned_at"`
}

// Assignment 由管理员指定目标版本和签名清单地址。
// 对同一目标设置 Retry 可重新下发已失败或已回滚的任务。
type Assignment struct {
	Version     string `json:"version"`
	ManifestURL string `json:"manifest_url"`
	Retry       bool   `json:"retry,omitempty"`
}

// Report 是设备对指定 AssignmentID 的进度回执。Error 仅用于失败或回滚状态。
type Report struct {
	AssignmentID string `json:"assignment_id"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}

// CheckRequest 上报设备当前运行的版本，并可同时提交一条更新回执。
type CheckRequest struct {
	Version string  `json:"version"`
	Report  *Report `json:"report,omitempty"`
}

// CheckResponse 返回可以继续处理的任务。没有待处理任务时 Target 为 nil。
type CheckResponse struct {
	Target *Target `json:"target"`
}

// DeviceState 是供管理端读取的最近更新状态。
// CurrentVersion 为空表示设备尚未上报版本。Target 可保留已结束任务的信息。
type DeviceState struct {
	DeviceID       string     `json:"device_id"`
	CurrentVersion string     `json:"current_version"`
	Target         *Target    `json:"target"`
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
	ReportedAt     *time.Time `json:"reported_at,omitempty"`
}

// ValidVersion 校验三个十进制版本分量。各分量最多九位，只有零本身可以以零开头。
func ValidVersion(version string) bool {
	return versionPattern.MatchString(version)
}

// CompareVersion 按数值比较两个已经过 ValidVersion 校验的版本，返回 -1、0 或 1。
func CompareVersion(left, right string) int {
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	for part, leftText := range leftParts {
		leftNumber, _ := strconv.Atoi(leftText)
		rightNumber, _ := strconv.Atoi(rightParts[part])
		if leftNumber < rightNumber {
			return -1
		}
		if leftNumber > rightNumber {
			return 1
		}
	}
	return 0
}

// ValidURL 校验 HTTP 或 HTTPS 制品地址的格式，要求包含主机并省略用户凭据和片段。
func ValidURL(value string) bool {
	if len(value) > 2048 || strings.ContainsAny(value, "\r\n\t #") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.Scheme == "https" || parsed.Scheme == "http"
}

// Validate 校验管理员提交的目标版本和签名清单地址。
func (assignment Assignment) Validate() error {
	if !ValidVersion(assignment.Version) {
		return errors.New("invalid target version")
	}
	if !ValidURL(assignment.ManifestURL) {
		return errors.New("invalid manifest URL")
	}
	return nil
}

// Validate 校验下发记录的任务 ID、下发时间、版本和清单地址。
func (target Target) Validate() error {
	if !assignmentPattern.MatchString(target.ID) || target.AssignedAt.IsZero() {
		return errors.New("invalid update assignment")
	}
	return (Assignment{Version: target.Version, ManifestURL: target.ManifestURL}).Validate()
}

// Validate 校验上报版本和可选回执的字段范围。任务归属与状态推进由 Hub 核对。
func (check CheckRequest) Validate() error {
	if !ValidVersion(check.Version) {
		return errors.New("invalid device version")
	}
	if check.Report == nil {
		return nil
	}
	report := check.Report
	if !assignmentPattern.MatchString(report.AssignmentID) {
		return errors.New("invalid update assignment ID")
	}
	switch report.Status {
	case StatusDownloading, StatusWaiting, StatusApplying, StatusSucceeded, StatusFailed, StatusRolledBack:
	default:
		return errors.New("invalid update progress")
	}
	if len(report.Error) > 512 || !utf8.ValidString(report.Error) ||
		strings.ContainsFunc(report.Error, unicode.IsControl) ||
		(report.Error != "" && report.Status != StatusFailed && report.Status != StatusRolledBack) {
		return errors.New("invalid update error")
	}
	return nil
}

// Terminal 判断更新任务是否已成功、失败或回滚。
func Terminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusRolledBack
}
