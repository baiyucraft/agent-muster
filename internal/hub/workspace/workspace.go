// Package workspace 定义 Hub 主对话、任务执行与设备协作的数据契约。
package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/baiyucraft/agent-muster/internal/approval"
	"github.com/baiyucraft/agent-muster/internal/artifact"
	"github.com/baiyucraft/agent-muster/internal/gitdelivery"
	"github.com/baiyucraft/agent-muster/internal/inputreceipt"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

var ErrConflict = errors.New("workspace state conflict")

// ErrInvalidApproval 表示请示或答复未通过输入校验。
var ErrInvalidApproval = errors.New("invalid approval or answer")

// ErrInvalidRunDiagnostics 表示运行诊断格式无效，或运行尚未结束。
var ErrInvalidRunDiagnostics = errors.New("invalid run diagnostics")

// ErrInvalidGitVerification 表示事件的 Git 验证回执无效，或不适用于当前运行状态。
var ErrInvalidGitVerification = errors.New("invalid Git verification")

// ErrInvalidGitPublication 表示推送证据或远端回读数据无效。
var ErrInvalidGitPublication = errors.New("invalid Git publication evidence")

// ErrInvalidInputReceipt 表示回执格式、输入归属或必需处理项未通过校验。
var ErrInvalidInputReceipt = errors.New("invalid input receipt")

// ErrInvalidArtifacts 表示产物引用不属于当前文件交付协议或未通过校验。
var ErrInvalidArtifacts = errors.New("invalid run artifacts")

// ErrInvalidSessionFailure 表示事件中的原生会话失败分类或附带证据无效。
var ErrInvalidSessionFailure = errors.New("invalid native session failure")

// Directory 表示设备登记的执行目录标识与展示名称。
type Directory struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	DeviceID string `json:"device_id,omitempty"`
}

// Project 表示项目及其执行绑定，顶层设备与目录字段记录默认绑定。
type Project struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	DeviceID       string              `json:"device_id"`
	DirectoryID    string              `json:"directory_id"`
	DirectoryLabel string              `json:"directory_label"`
	AgentKind      string              `json:"agent_kind"`
	Bindings       []Binding           `json:"bindings,omitempty"`
	GitPolicy      *gitdelivery.Policy `json:"git_policy,omitempty"`
	ClearGitPolicy bool                `json:"-"` // 管理端显式提交 null 时清空规则。
}

// Binding 将项目关联到指定设备、目录和 Agent，并记录是否为默认项。
type Binding struct {
	ID             string `json:"id"`
	DeviceID       string `json:"device_id"`
	DirectoryID    string `json:"directory_id"`
	DirectoryLabel string `json:"directory_label"`
	AgentKind      string `json:"agent_kind"`
	IsDefault      bool   `json:"is_default"`
}

// Message 表示主对话中的消息及其任务关联和处理状态。
type Message struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text"`
	TaskID    string    `json:"task_id,omitempty"`
	Status    string    `json:"status,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Task 表示带有冻结执行绑定和运行代次的任务，可关联父任务与根任务。
type Task struct {
	ID                   string    `json:"id"`
	ProjectID            string    `json:"project_id"`
	ProjectName          string    `json:"project_name"`
	DeviceID             string    `json:"device_id"`
	DirectoryID          string    `json:"directory_id"`
	Status               string    `json:"status"`
	Title                string    `json:"title"`
	Result               string    `json:"result,omitempty"`
	ReportPending        bool      `json:"report_pending"`
	UpdatedAt            time.Time `json:"updated_at"`
	ParentTaskID         string    `json:"parent_task_id,omitempty"`
	RootTaskID           string    `json:"root_task_id,omitempty"`
	BindingID            string    `json:"binding_id,omitempty"`
	AgentKind            string    `json:"agent_kind"`
	WaitingReason        string    `json:"waiting_reason,omitempty"`
	CurrentRunGeneration int64     `json:"current_run_generation"`
	WaitGeneration       int64     `json:"wait_generation,omitempty"`
	ControlActions       []string  `json:"control_actions,omitempty"`
	GitPolicyRevision    int64     `json:"git_policy_revision,omitempty"`
	GitPublish           bool      `json:"git_publish,omitempty"`
	CenterGroup          bool      `json:"center_group,omitempty"`            // 中心聚合任务没有设备运行。
	DependsOnTaskIDs     []string  `json:"depends_on_task_ids,omitempty"`     // 中心计划定义的固定前置步骤。
	TakeoverSourceTaskID string    `json:"takeover_source_task_id,omitempty"` // 重新接手时关联的原任务。
}

// TaskControl 是管理员任务操作。请求 ID 在同一任务下去重，重复时返回当前状态。
type TaskControl struct {
	Action    string `json:"action"`
	RequestID string `json:"request_id"`
}

// TaskTakeover 是管理员确认的接手摘要。同一来源任务按请求 ID 和摘要去重。
type TaskTakeover struct {
	RequestID string `json:"request_id"`
	Summary   string `json:"summary"`
}

// Run 描述任务的一轮执行，保留其状态、原生会话、租约及设备上报的诊断摘要。
type Run struct {
	ID              string                    `json:"id"`
	TaskID          string                    `json:"task_id"`
	DirectoryID     string                    `json:"directory_id"`
	Status          string                    `json:"status"`
	SessionID       string                    `json:"session_id,omitempty"`
	Text            string                    `json:"text,omitempty"`
	CreatedAt       time.Time                 `json:"created_at"`
	UpdatedAt       time.Time                 `json:"updated_at"`
	Generation      int64                     `json:"generation"`
	LeaseUntil      *time.Time                `json:"lease_until,omitempty"`
	Diagnostics     *runreport.Diagnostics    `json:"diagnostics,omitempty"`
	GitVerification *gitdelivery.Verification `json:"git_verification,omitempty"`
	GitPublication  *gitdelivery.Publication  `json:"git_publication,omitempty"`
}

// RunClaim 是设备领取运行后的数据，包含执行上下文、租约、输入和可委派目标。
type RunClaim struct {
	ID                      string             `json:"id"`
	TaskID                  string             `json:"task_id"`
	DirectoryID             string             `json:"directory_id"`
	Prompt                  string             `json:"prompt"`
	SessionID               string             `json:"session_id"`
	SessionFailureSupported bool               `json:"session_failure_supported,omitempty"` // Hub 可处理原生续接开始前的分类故障。
	AgentKind               string             `json:"agent_kind"`
	ProjectID               string             `json:"project_id"`
	Generation              int64              `json:"generation"`
	Lease                   string             `json:"lease"`
	LeaseUntil              time.Time          `json:"lease_until"`
	BindingID               string             `json:"binding_id"`
	ParentTaskID            string             `json:"parent_task_id,omitempty"`
	ParentDeviceID          string             `json:"parent_device_id,omitempty"`
	RootTaskID              string             `json:"root_task_id"`
	PromptTransferID        string             `json:"prompt_transfer_id,omitempty"`
	Attachments             []Transfer         `json:"attachments,omitempty"`
	DependencyResults       []DependencyResult `json:"dependency_results,omitempty"`
	InputIDs                []string           `json:"input_ids,omitempty"`      // 首次领取时冻结的输入。
	InputMessages           map[string]string  `json:"input_messages,omitempty"` // 续接时带回的未处理消息，按输入 ID 索引。
	DelegationTargets       []DelegationTarget `json:"delegation_targets,omitempty"`
	ApprovalSupported       bool               `json:"approval_supported,omitempty"`
	ControlSupported        bool               `json:"control_supported,omitempty"`
	DiagnosticsSupported    bool               `json:"diagnostics_supported,omitempty"`
	ArtifactsSupported      bool               `json:"artifacts_supported,omitempty"` // Hub 接受本轮的本地产物引用。
	// ReviewDiagnosticsSupported 声明当前 Hub 可接收独立 Review 诊断。
	ReviewDiagnosticsSupported bool                `json:"review_diagnostics_supported,omitempty"`
	GitPolicy                  *gitdelivery.Policy `json:"git_policy,omitempty"`
	GitPublish                 bool                `json:"git_publish,omitempty"`
}

// RunClaimCapabilities 记录设备在领取请求中声明支持的运行协议。
type RunClaimCapabilities struct {
	Control            bool
	GitVerification    bool
	GitPublish         bool
	InputReceipt       bool
	GitCandidateCommit bool
}

// DependencyResult 记录父任务续接时收到的子任务标识和状态。
type DependencyResult struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// DelegationTarget 描述能接收子任务的其他设备与项目绑定。
type DelegationTarget struct {
	ProjectID      string `json:"project_id"`
	ProjectName    string `json:"project_name"`
	BindingID      string `json:"binding_id"`
	DeviceID       string `json:"device_id"`
	DeviceName     string `json:"device_name"`
	AgentKind      string `json:"agent_kind"`
	DirectoryLabel string `json:"directory_label"`
}

// RunEvent 是设备上报的单条运行事件，携带运行代次和租约。诊断摘要只随本轮终态上报。
type RunEvent struct {
	Kind            string                    `json:"kind"`
	SessionFailure  string                    `json:"session_failure,omitempty"` // dsh 原生续接启动前的失败分类：busy 或 unavailable。
	SessionID       string                    `json:"session_id,omitempty"`
	Text            string                    `json:"text,omitempty"`
	Generation      int64                     `json:"generation"`
	Lease           string                    `json:"lease"`
	WaitForTaskIDs  []string                  `json:"wait_for_task_ids,omitempty"`
	TransferIDs     []string                  `json:"transfer_ids,omitempty"`
	Approval        *approval.Question        `json:"approval,omitempty"`
	Diagnostics     *runreport.Diagnostics    `json:"diagnostics,omitempty"`
	GitVerification *gitdelivery.Verification `json:"git_verification,omitempty"`
	InputReceipt    *inputreceipt.Receipt     `json:"input_receipt,omitempty"` // 本轮确认处理的输入。
	Artifacts       []artifact.Reference      `json:"artifacts,omitempty"`     // 已在设备保存的本地快照。
}

// Approval 记录原生会话提出的请示、首个有效答复及处理时间。
// ID 与主对话消息一致，TaskID 指向发起请示的任务，可能是子任务。
type Approval struct {
	ID         string     `json:"id"`
	TaskID     string     `json:"task_id"`
	RootTaskID string     `json:"root_task_id"`
	RunID      string     `json:"run_id"`
	Question   string     `json:"question"`
	Options    []string   `json:"options"`
	Impact     string     `json:"impact,omitempty"`
	Status     string     `json:"status"`
	Answer     string     `json:"answer,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`
	HandledAt  *time.Time `json:"handled_at,omitempty"`
	// WaitingDevice 说明首答已保存，原会话续接尚未被离线设备领取。
	WaitingDevice bool `json:"waiting_device,omitempty"`
}

// Snapshot 是工作区在同一事件游标位置上的对话、任务、项目和目录快照。
type Snapshot struct {
	Cursor           int64             `json:"cursor"`
	Messages         []Message         `json:"messages"`
	Tasks            []Task            `json:"tasks"`
	Projects         []Project         `json:"projects"`
	Directories      []Directory       `json:"directories"`
	Approvals        []Approval        `json:"approvals"`
	BindingProposals []BindingProposal `json:"binding_proposals"`
}

// Detail 汇集单项任务的运行记录、子任务、依赖、输入和投递。
type Detail struct {
	Task            Task           `json:"task"`
	Runs            []Run          `json:"runs"`
	Messages        []Message      `json:"messages"`
	Children        []Task         `json:"children"`
	Dependencies    []Dependency   `json:"dependencies"`
	Transfers       []Transfer     `json:"transfers"`
	Inputs          []TaskInput    `json:"inputs"`
	WaitedTransfers []TransferWait `json:"waited_transfers"`
	Approvals       []Approval     `json:"approvals"`
	Artifacts       []TaskArtifact `json:"artifacts,omitempty"`
	Takeovers       []Task         `json:"takeovers,omitempty"`
	TakeoverAllowed bool           `json:"takeover_allowed,omitempty"`
}

// TaskArtifact 是 Hub 确认的设备文件版本。任务、运行及设备由已认证的运行记录确定。
type TaskArtifact struct {
	artifact.Reference
	TaskID    string    `json:"task_id"`
	RunID     string    `json:"run_id"`
	DeviceID  string    `json:"device_id"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskInput 记录任务输入的来源、消费状态与关联投递。
type TaskInput struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	MessageID     string     `json:"message_id,omitempty"`
	RunID         string     `json:"run_id,omitempty"`
	Generation    int64      `json:"generation,omitempty"`
	TransferIDs   []string   `json:"transfer_ids,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
	ReceiptStatus string     `json:"receipt_status,omitempty"` // 支持回执协议的输入处理状态。
	ReceiptRunID  string     `json:"receipt_run_id,omitempty"` // 最近一次投递或确认处理该输入的轮次。
	HandledAt     *time.Time `json:"handled_at,omitempty"`     // 处理回执入库时间。
}

// TransferWait 记录一笔投递在指定等待代次中的签收状态。
type TransferWait struct {
	TransferID string     `json:"transfer_id"`
	Generation int64      `json:"generation"`
	Active     bool       `json:"active"`
	Status     string     `json:"status"`
	ReceivedAt *time.Time `json:"received_at,omitempty"`
}

// Dependency 记录父任务在指定等待代次中依赖的直接子任务。
type Dependency struct {
	TaskID          string `json:"task_id"`
	DependsOnTaskID string `json:"depends_on_task_id"`
	Generation      int64  `json:"generation"`
	Status          string `json:"status"`
	Active          bool   `json:"active"`
}

// Delivery 表示中心插件领取的待处理消息或根任务汇报，包含处理租约。
type Delivery struct {
	ID        string `json:"id"`
	Lease     string `json:"lease"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	MessageID string `json:"message_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Attempt   int    `json:"attempt"`
	// UsageSupported 表示当前 Hub 接受带中心轮次用量的完成回执。
	UsageSupported bool `json:"usage_supported,omitempty"`
	// PlanSupported 表示当前 Hub 支持在一次派发中保存带依赖的任务组。
	PlanSupported bool `json:"plan_supported,omitempty"`
	// BindingProposalsSupported 表示当前 Hub 可登记待管理员确认的新建项目提案。
	BindingProposalsSupported bool `json:"binding_proposals_supported,omitempty"`
	// BindingProposal 恢复当前消息此前已登记的提案和决定。
	BindingProposal *BindingProposal `json:"binding_proposal,omitempty"`
}

// Dispatch 表示中心插件依据消息租约创建任务或追加运行的请求。
type Dispatch struct {
	Lease     string         `json:"lease"`
	ProjectID string         `json:"project_id,omitempty"`
	TaskID    string         `json:"task_id,omitempty"`
	Title     string         `json:"title,omitempty"`
	Prompt    string         `json:"prompt"`
	BindingID string         `json:"binding_id,omitempty"`
	Steps     []DispatchStep `json:"steps,omitempty"`
	// GitDelivery 请求为新根任务冻结已配置的项目 Git 规则。
	GitDelivery bool `json:"git_delivery,omitempty"`
	// GitPublish 明确请求向冻结规则中的目标分支推送。
	GitPublish bool `json:"git_publish,omitempty"`
}

// DispatchStep 描述一项设备工作。DependsOn 从 1 开始引用前面的步骤。
type DispatchStep struct {
	ProjectID string `json:"project_id"`
	BindingID string `json:"binding_id,omitempty"`
	Title     string `json:"title"`
	Prompt    string `json:"prompt"`
	DependsOn []int  `json:"depends_on,omitempty"`
}

// Peer 保存设备直连接收端点的地址及证书指纹。
type Peer struct {
	DeviceID    string `json:"device_id"`
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
}

// TransferRequest 指定发送运行的凭据、接收设备和待投递内容的摘要及大小。
type TransferRequest struct {
	Lease          string `json:"lease"`
	Generation     int64  `json:"generation"`
	ID             string `json:"id"`
	TargetDeviceID string `json:"target_device_id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
}

// Transfer 记录设备间投递的元数据、票据、失败原因和接收确认。
type Transfer struct {
	ID             string     `json:"id"`
	RunID          string     `json:"run_id"`
	TaskID         string     `json:"task_id"`
	SourceDeviceID string     `json:"source_device_id"`
	TargetDeviceID string     `json:"target_device_id"`
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`
	SHA256         string     `json:"sha256"`
	Size           int64      `json:"size"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	LastError      string     `json:"last_error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ReceivedAt     *time.Time `json:"received_at,omitempty"`
	Ticket         string     `json:"ticket,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	Peer           *Peer      `json:"peer,omitempty"`
}

// Delegation 请求当前运行以稳定请求键创建子任务，并关联其输入投递。
type Delegation struct {
	Generation       int64    `json:"generation"`
	Lease            string   `json:"lease"`
	RequestKey       string   `json:"request_key"`
	TaskID           string   `json:"task_id"`
	ProjectID        string   `json:"project_id"`
	BindingID        string   `json:"binding_id"`
	Title            string   `json:"title"`
	PromptTransferID string   `json:"prompt_transfer_id"`
	TransferIDs      []string `json:"transfer_ids,omitempty"`
}

// Completion 是中心插件对已领取消息或汇报提交的处理回执。
type Completion struct {
	Lease  string `json:"lease"`
	Text   string `json:"text"`
	Status string `json:"status"`
	// Usage 保存本投递确认的中心 dsh 轮次计量，缺失时保持未知。
	Usage *runreport.Usage `json:"usage,omitempty"`
}

// Store 定义工作区快照、任务执行、设备协作和中心插件投递的持久化操作。
type Store interface {
	Workspace(context.Context) (Snapshot, error)
	DeviceUsage(context.Context) (DeviceUsage, error)
	WorkspaceEvents(context.Context, int64) ([]int64, error)
	TaskDetail(context.Context, string) (Detail, error)
	Artifact(context.Context, string, string) (TaskArtifact, error)
	RunLogDevice(context.Context, string, string) (string, error)
	AuthenticateRunLogDevice(context.Context, string, [32]byte) error
	ControlTask(context.Context, string, TaskControl) (Task, error)
	TakeOverTask(context.Context, string, TaskTakeover) (Task, error)
	SaveProject(context.Context, Project) (Project, error)
	Projects(context.Context) ([]Project, error)
	BindingCandidates(context.Context) ([]BindingCandidate, error)
	ProposeBinding(context.Context, string, BindingProposalInput) (BindingProposal, error)
	DecideBinding(context.Context, string, string) (BindingProposal, error)
	SubmitMessage(context.Context, Message) (Message, error)
	AnswerApproval(context.Context, string, string) (Approval, error)
	ReplaceDirectories(context.Context, string, [32]byte, []Directory) error
	ClaimRun(context.Context, string, [32]byte) (RunClaim, error)
	ClaimRunWithControl(context.Context, string, [32]byte, bool) (RunClaim, error)
	ClaimRunWithCapabilities(context.Context, string, [32]byte, bool, bool) (RunClaim, error)
	ClaimCompatibleRun(context.Context, string, [32]byte, RunClaimCapabilities) (RunClaim, error)
	AuthorizeGitPush(context.Context, string, [32]byte, string, gitdelivery.PushRequest) (gitdelivery.PushIntent, error)
	ObserveGitPush(context.Context, string, [32]byte, string, string, gitdelivery.Observation) (gitdelivery.PushIntent, error)
	RunControl(context.Context, string, [32]byte, string, int64, string) (bool, error)
	RenewRun(context.Context, string, [32]byte, string, int64, string) (time.Time, error)
	Delegate(context.Context, string, [32]byte, string, Delegation) (Task, error)
	ValidateWait(context.Context, string, [32]byte, string, RunEvent) error
	RecordRunEvent(context.Context, string, [32]byte, string, int64, RunEvent) error
	RegisterPeer(context.Context, string, [32]byte, Peer) error
	CreateTransfer(context.Context, string, [32]byte, string, TransferRequest) (Transfer, error)
	OutgoingTransfers(context.Context, string, [32]byte) ([]Transfer, error)
	AuthorizeTransfer(context.Context, string, [32]byte, string) (Transfer, error)
	VerifyTransfer(context.Context, string, [32]byte, string, string) (Transfer, error)
	AcknowledgeTransfer(context.Context, string, [32]byte, string, string) error
	ReportTransferError(context.Context, string, [32]byte, string, string) error
	ClaimDelivery(context.Context) (Delivery, error)
	RenewDelivery(context.Context, string, string) error
	CompleteDelivery(context.Context, string, Completion) error
	Dispatch(context.Context, string, Dispatch) (string, error)
}
