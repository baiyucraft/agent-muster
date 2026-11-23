// Package workspace 定义 Hub 主对话、任务执行与设备协作的数据契约。
package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/baiyucraft/agent-muster/internal/approval"
)

var ErrConflict = errors.New("workspace state conflict")

// ErrInvalidApproval 表示请示或答复未通过输入校验。
var ErrInvalidApproval = errors.New("invalid approval or answer")

// Directory 表示设备登记的执行目录标识与展示名称。
type Directory struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	DeviceID string `json:"device_id,omitempty"`
}

// Project 表示项目及其执行绑定，顶层设备与目录字段记录默认绑定。
type Project struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	DeviceID       string    `json:"device_id"`
	DirectoryID    string    `json:"directory_id"`
	DirectoryLabel string    `json:"directory_label"`
	AgentKind      string    `json:"agent_kind"`
	Bindings       []Binding `json:"bindings,omitempty"`
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
}

// Run 描述任务的一轮执行，保留其状态、原生会话、代次和租约期限。
type Run struct {
	ID          string     `json:"id"`
	TaskID      string     `json:"task_id"`
	DirectoryID string     `json:"directory_id"`
	Status      string     `json:"status"`
	SessionID   string     `json:"session_id,omitempty"`
	Text        string     `json:"text,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Generation  int64      `json:"generation"`
	LeaseUntil  *time.Time `json:"lease_until,omitempty"`
}

// RunClaim 是设备领取运行后的数据，包含执行上下文、租约、输入和可委派目标。
type RunClaim struct {
	ID                string             `json:"id"`
	TaskID            string             `json:"task_id"`
	DirectoryID       string             `json:"directory_id"`
	Prompt            string             `json:"prompt"`
	SessionID         string             `json:"session_id"`
	AgentKind         string             `json:"agent_kind"`
	ProjectID         string             `json:"project_id"`
	Generation        int64              `json:"generation"`
	Lease             string             `json:"lease"`
	LeaseUntil        time.Time          `json:"lease_until"`
	BindingID         string             `json:"binding_id"`
	ParentTaskID      string             `json:"parent_task_id,omitempty"`
	ParentDeviceID    string             `json:"parent_device_id,omitempty"`
	RootTaskID        string             `json:"root_task_id"`
	PromptTransferID  string             `json:"prompt_transfer_id,omitempty"`
	Attachments       []Transfer         `json:"attachments,omitempty"`
	DependencyResults []DependencyResult `json:"dependency_results,omitempty"`
	DelegationTargets []DelegationTarget `json:"delegation_targets,omitempty"`
	ApprovalSupported bool               `json:"approval_supported,omitempty"`
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

// RunEvent 是设备上报的单条运行事件，携带运行代次和租约。
type RunEvent struct {
	Kind           string             `json:"kind"`
	SessionID      string             `json:"session_id,omitempty"`
	Text           string             `json:"text,omitempty"`
	Generation     int64              `json:"generation"`
	Lease          string             `json:"lease"`
	WaitForTaskIDs []string           `json:"wait_for_task_ids,omitempty"`
	TransferIDs    []string           `json:"transfer_ids,omitempty"`
	Approval       *approval.Question `json:"approval,omitempty"`
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
}

// Snapshot 是工作区在同一事件游标位置上的对话、任务、项目和目录快照。
type Snapshot struct {
	Cursor      int64       `json:"cursor"`
	Messages    []Message   `json:"messages"`
	Tasks       []Task      `json:"tasks"`
	Projects    []Project   `json:"projects"`
	Directories []Directory `json:"directories"`
	Approvals   []Approval  `json:"approvals"`
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
}

// TaskInput 记录任务输入的来源、消费状态与关联投递。
type TaskInput struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	MessageID   string     `json:"message_id,omitempty"`
	RunID       string     `json:"run_id,omitempty"`
	Generation  int64      `json:"generation,omitempty"`
	TransferIDs []string   `json:"transfer_ids,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ClaimedAt   *time.Time `json:"claimed_at,omitempty"`
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
}

// Dispatch 表示中心插件依据消息租约创建任务或追加运行的请求。
type Dispatch struct {
	Lease     string `json:"lease"`
	ProjectID string `json:"project_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	Title     string `json:"title,omitempty"`
	Prompt    string `json:"prompt"`
	BindingID string `json:"binding_id,omitempty"`
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
}

// Store 定义工作区快照、任务执行、设备协作和中心插件投递的持久化操作。
type Store interface {
	Workspace(context.Context) (Snapshot, error)
	WorkspaceEvents(context.Context, int64) ([]int64, error)
	TaskDetail(context.Context, string) (Detail, error)
	SaveProject(context.Context, Project) (Project, error)
	Projects(context.Context) ([]Project, error)
	SubmitMessage(context.Context, Message) (Message, error)
	AnswerApproval(context.Context, string, string) (Approval, error)
	ReplaceDirectories(context.Context, string, [32]byte, []Directory) error
	ClaimRun(context.Context, string, [32]byte) (RunClaim, error)
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
