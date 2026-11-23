package workspace

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
)

// ErrInvalidBindingProposal 表示提案或决定的结构无效。
var ErrInvalidBindingProposal = errors.New("invalid binding proposal")

// MaxProposedBindings 采用管理页的新建项目绑定数量上限。
const MaxProposedBindings = 32

var proposedDirectoryID = regexp.MustCompile(`^dir-[0-9a-f]{32}$`)

// BindingCandidate 列出设备登记的目录和最近一次 CLI 探测结果。
type BindingCandidate struct {
	DeviceID    string           `json:"device_id"`
	DeviceName  string           `json:"device_name"`
	LastSeenAt  *time.Time       `json:"last_seen_at,omitempty"`
	Directories []Directory      `json:"directories"`
	Agents      []registry.Agent `json:"agents"`
}

// ProposedBinding 确定设备、目录和 Agent 的组合。展示名称由 Hub 从登记信息补齐。
type ProposedBinding struct {
	DeviceID       string `json:"device_id"`
	DeviceName     string `json:"device_name,omitempty"`
	DirectoryID    string `json:"directory_id"`
	DirectoryLabel string `json:"directory_label,omitempty"`
	AgentKind      string `json:"agent_kind"`
	IsDefault      bool   `json:"is_default"`
}

// BindingProposal 保存来源消息、待确认的目录绑定及管理员决定。
type BindingProposal struct {
	ID        string            `json:"id"`
	MessageID string            `json:"message_id"`
	Name      string            `json:"name"`
	Bindings  []ProposedBinding `json:"bindings"`
	Status    string            `json:"status"`
	ProjectID string            `json:"project_id,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	DecidedAt *time.Time        `json:"decided_at,omitempty"`
}

// BindingProposalInput 携带当前中心消息的租约及待确认的新项目绑定。
type BindingProposalInput struct {
	Lease    string            `json:"lease"`
	Name     string            `json:"name"`
	Bindings []ProposedBinding `json:"bindings"`
}

// Normalize 校验项目名、目录与 CLI 标识、唯一目标和默认项，移除调用方提供的展示名称。
func (input BindingProposalInput) Normalize() (BindingProposalInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 120 || !utf8.ValidString(input.Name) || strings.ContainsRune(input.Name, 0) ||
		len(input.Bindings) == 0 || len(input.Bindings) > MaxProposedBindings {
		return BindingProposalInput{}, ErrInvalidBindingProposal
	}
	bindings := make([]ProposedBinding, 0, len(input.Bindings))
	type target struct{ device, directory, agent string }
	seen := make(map[target]bool, len(input.Bindings))
	defaults := 0
	for _, binding := range input.Bindings {
		key := target{binding.DeviceID, binding.DirectoryID, binding.AgentKind}
		if seen[key] || !registry.ValidDeviceID(binding.DeviceID) || !proposedDirectoryID.MatchString(binding.DirectoryID) ||
			(binding.AgentKind != "dsh" && binding.AgentKind != "codex") {
			return BindingProposalInput{}, ErrInvalidBindingProposal
		}
		seen[key] = true
		if binding.IsDefault {
			defaults++
		}
		binding.DeviceName, binding.DirectoryLabel = "", ""
		bindings = append(bindings, binding)
	}
	if defaults != 1 {
		return BindingProposalInput{}, ErrInvalidBindingProposal
	}
	input.Bindings = bindings
	return input, nil
}
