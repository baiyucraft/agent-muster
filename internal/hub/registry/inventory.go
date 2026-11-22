// Package registry 定义设备清单和 Hub 对上报数据的校验规则。
package registry

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// OnlineWindow 是 Hub 从最近一次成功心跳起认定设备在线的时长。
	OnlineWindow = 90 * time.Second
	// PairingWindow 是配对码从创建起可使用的时长。
	PairingWindow = 10 * time.Minute
)

var (
	// ErrInvalidPairing 表示配对码不可使用。
	ErrInvalidPairing = errors.New("invalid pairing code")
	// ErrUnauthorized 表示设备凭据无法用于此设备。
	ErrUnauthorized = errors.New("invalid device credentials")
	// ErrNotFound 表示此设备尚未登记。
	ErrNotFound = errors.New("device not found")
)

var (
	deviceIDPattern = regexp.MustCompile(`^d-[0-9a-f]{32}$`)
	platformPattern = regexp.MustCompile(`^[a-z0-9]{1,32}$`)
	versionPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
)

// Capabilities 描述设备对本机 CLI 接口的探测结果。
type Capabilities struct {
	Prompt     bool `json:"prompt"`
	FinalReply bool `json:"final_reply"`
	Resume     bool `json:"resume"`
}

// Agent 描述已知 Agent CLI 的本机探测结果。
type Agent struct {
	ID           string        `json:"id"`
	Version      string        `json:"version"`
	Status       string        `json:"status"`
	Capabilities *Capabilities `json:"capabilities"`
}

// Inventory 是设备最近一次报告的完整 Agent 清单。
type Inventory struct {
	Agents []Agent `json:"agents"`
}

// Enrollment 是设备配对时报告的本机信息。
type Enrollment struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// Device 是已登记设备、最近的清单快照及基于心跳计算的在线状态。
type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Enrollment
	Inventory
	RegisteredAt time.Time  `json:"registered_at"`
	LastSeenAt   *time.Time `json:"last_seen_at"`
	ProbedAt     *time.Time `json:"probed_at"`
	RevokedAt    *time.Time `json:"revoked_at"`
	Online       bool       `json:"online"`
}

// Store 保存配对、设备登记、心跳和最后的完整清单。凭据以 SHA-256 摘要传入。
type Store interface {
	CreatePairing(context.Context, string, [32]byte) (time.Time, error)
	Enroll(context.Context, string, Enrollment, [32]byte, [32]byte) error
	Heartbeat(context.Context, string, [32]byte) error
	Report(context.Context, string, [32]byte, Inventory) error
	List(context.Context) ([]Device, error)
	Revoke(context.Context, string) error
}

// ValidDeviceID 校验用于 URL 和长期登记的设备标识。
func ValidDeviceID(id string) bool {
	return deviceIDPattern.MatchString(id)
}

// ValidName 校验管理端设备名称或设备上报的主机名。
func ValidName(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.ValidString(name) &&
		utf8.RuneCountInString(name) <= 128 && !strings.ContainsFunc(name, unicode.IsControl)
}

// Validate 校验设备的主机名、系统和处理器架构。
func (enrollment Enrollment) Validate() error {
	if !ValidName(enrollment.Hostname) {
		return errors.New("invalid hostname")
	}
	if !platformPattern.MatchString(enrollment.OS) || !platformPattern.MatchString(enrollment.Arch) {
		return errors.New("invalid device platform")
	}
	return nil
}

// Validate 校验清单中的 Agent 标识、版本和 CLI 探测结果。
func (inventory Inventory) Validate() error {
	if inventory.Agents == nil || len(inventory.Agents) > 2 {
		return errors.New("invalid agents list")
	}

	seen := make(map[string]bool, len(inventory.Agents))
	for _, agent := range inventory.Agents {
		if (agent.ID != "codex" && agent.ID != "dsh") || seen[agent.ID] {
			return errors.New("invalid or duplicate agent ID")
		}
		seen[agent.ID] = true
		if agent.Version != "" && (len(agent.Version) > 64 || !versionPattern.MatchString(agent.Version)) {
			return errors.New("invalid agent version")
		}
		if agent.Capabilities == nil {
			return errors.New("agent capabilities required")
		}
		capabilities := *agent.Capabilities
		switch agent.Status {
		case "supported":
			if agent.Version == "" || !(capabilities.Prompt && capabilities.FinalReply && capabilities.Resume) {
				return errors.New("supported agent requires a version and all capabilities")
			}
		case "missing":
			if agent.Version != "" || capabilities != (Capabilities{}) {
				return errors.New("missing agent cannot have a version or capabilities")
			}
		case "unsupported", "probe_failed":
		default:
			return errors.New("invalid agent status")
		}
	}
	return nil
}
