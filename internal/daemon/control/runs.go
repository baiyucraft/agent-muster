package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// HTTPError 记录 Hub 返回的非预期 HTTP 状态码。
type HTTPError struct {
	Status int
}

// Error 返回只含 Hub 状态码的错误消息。
func (err *HTTPError) Error() string {
	return fmt.Sprintf("Hub 返回 HTTP %d", err.Status)
}

// Directory 是上报给 Hub 的工作目录标识和显示名。
type Directory struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Run 携带 Hub 领取结果中的任务关系、冻结绑定、输入和执行租约。
type Run struct {
	ID                string              `json:"id"`
	TaskID            string              `json:"task_id"`
	RootTaskID        string              `json:"root_task_id"`
	ParentTaskID      string              `json:"parent_task_id"`
	ParentDeviceID    string              `json:"parent_device_id"`
	ProjectID         string              `json:"project_id"`
	BindingID         string              `json:"binding_id"`
	AgentKind         string              `json:"agent_kind"`
	DirectoryID       string              `json:"directory_id"`
	Prompt            string              `json:"prompt"`
	SessionID         string              `json:"session_id"`
	PromptTransferID  string              `json:"prompt_transfer_id"`
	Generation        int64               `json:"generation"`
	Lease             string              `json:"lease"`
	LeaseUntil        string              `json:"lease_until"`
	Attachments       []TransferReference `json:"attachments"`
	DependencyResults []DependencyResult  `json:"dependency_results,omitempty"`
	DelegationTargets []DelegationTarget  `json:"delegation_targets,omitempty"`
}

// DependencyResult 记录父任务续接时可见的依赖任务标识与状态。
type DependencyResult struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// DelegationTarget 描述当前运行可指派子任务的项目和设备绑定。
type DelegationTarget struct {
	ProjectID      string `json:"project_id"`
	ProjectName    string `json:"project_name"`
	BindingID      string `json:"binding_id"`
	DeviceID       string `json:"device_id"`
	DeviceName     string `json:"device_name"`
	AgentKind      string `json:"agent_kind"`
	DirectoryLabel string `json:"directory_label"`
}

// Peer 描述接收设备的直连地址和用于校验证书的指纹。
type Peer struct {
	DeviceID    string `json:"device_id"`
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
}

// Transfer 描述 Hub 保存的投递元数据，以及授权时返回的票据和接收端地址。
type Transfer struct {
	ID             string `json:"id"`
	RunID          string `json:"run_id"`
	TaskID         string `json:"task_id"`
	SourceDeviceID string `json:"source_device_id"`
	TargetDeviceID string `json:"target_device_id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
	Ticket         string `json:"ticket,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	ReceivedAt     string `json:"received_at,omitempty"`
	Peer           *Peer  `json:"peer,omitempty"`
}

// TransferReference 在运行输入中引用已有投递，并可附带本机引用。
type TransferReference struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	LocalRef string `json:"local_ref,omitempty"`
}

// RunEvent 是设备上报的运行事件，可附当前租约、会话或等待依赖。
type RunEvent struct {
	Kind           string   `json:"kind"`
	SessionID      string   `json:"session_id,omitempty"`
	Text           string   `json:"text,omitempty"`
	Generation     int64    `json:"generation,omitempty"`
	Lease          string   `json:"lease,omitempty"`
	WaitForTaskIDs []string `json:"wait_for_task_ids,omitempty"`
	TransferIDs    []string `json:"transfer_ids,omitempty"`
}

// ReportDirectories 向 Hub 上报本设备当前可派发的工作目录清单。
func (client *Client) ReportDirectories(ctx context.Context, identity Identity, directories []Directory) error {
	return client.report(ctx, identity, http.MethodPut, "directories", struct {
		Directories []Directory `json:"directories"`
	}{Directories: directories})
}

// ClaimRun 向 Hub 领取设备可执行的运行。没有可领取的运行时返回 nil。
func (client *Client) ClaimRun(ctx context.Context, identity Identity) (*Run, error) {
	endpoint, err := client.deviceURL(identity, "runs", "claim")
	if err != nil {
		return nil, err
	}
	response, err := client.send(ctx, http.MethodPost, endpoint, identity.Token, nil, http.StatusOK, http.StatusNoContent)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	var run Run
	const maxRunSize = 256 * 1024
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxRunSize+1))
	if err != nil || len(payload) > maxRunSize || json.Unmarshal(payload, &run) != nil {
		return nil, errors.New("Hub 的运行响应格式无效")
	}
	if run.ID == "" || run.TaskID == "" || run.DirectoryID == "" || (run.Prompt == "" && run.PromptTransferID == "" && len(run.Attachments) == 0) {
		return nil, errors.New("Hub 返回不完整的运行记录")
	}
	return &run, nil
}

// ReportRunEvent 按运行 ID 和设备事件序号上报事件，重试时复用这两个值。
func (client *Client) ReportRunEvent(ctx context.Context, identity Identity, runID string, sequence int64, event RunEvent) error {
	endpoint, err := client.deviceURL(identity, "runs", runID, "events", strconv.FormatInt(sequence, 10))
	if err != nil {
		return err
	}
	response, err := client.send(ctx, http.MethodPut, endpoint, identity.Token, event, http.StatusNoContent)
	if err == nil {
		response.Body.Close()
	}
	return err
}

func (client *Client) deviceURL(identity Identity, parts ...string) (*url.URL, error) {
	if !identity.Valid() || identity.HubURL != client.base.String() {
		return nil, errors.New("保存的设备凭据格式无效")
	}
	endpoint := client.base.JoinPath("api", "v1", "devices", identity.DeviceID)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || url.PathEscape(part) != part {
			return nil, errors.New("无效的设备接口路径")
		}
	}
	return endpoint.JoinPath(parts...), nil
}
