package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const MaxPeerBody = 32 << 20

// LeaseProof 标识发起协作请求的执行代次及其租约。
type LeaseProof struct {
	Generation int64  `json:"generation"`
	Lease      string `json:"lease"`
}

// Manifest 登记投递目标和正文摘要，供 Hub 授权和接收设备校验。
type Manifest struct {
	LeaseProof
	ID             string `json:"id"`
	TargetDeviceID string `json:"target_device_id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
}

// Delegation 记录父运行的子任务请求，包含重试时复用的请求键及输入投递引用。
type Delegation struct {
	LeaseProof
	RequestKey       string   `json:"request_key"`
	TaskID           string   `json:"task_id"`
	ProjectID        string   `json:"project_id"`
	BindingID        string   `json:"binding_id"`
	Title            string   `json:"title"`
	PromptTransferID string   `json:"prompt_transfer_id"`
	TransferIDs      []string `json:"transfer_ids"`
}

// RegisterPeer 向 Hub 登记本设备的直连地址和证书指纹。
func (client *Client) RegisterPeer(ctx context.Context, id Identity, address, fingerprint string) error {
	return client.collaboration(ctx, id, http.MethodPut, map[string]string{"address": address, "fingerprint": fingerprint}, nil, "peer")
}

// RenewRun 提交当前代次的租约凭据，并返回 Hub 确认的新到期时间。
func (client *Client) RenewRun(ctx context.Context, id Identity, run string, proof LeaseProof) (time.Time, error) {
	var result struct {
		LeaseUntil time.Time `json:"lease_until"`
	}
	err := client.collaboration(ctx, id, http.MethodPut, proof, &result, "runs", run, "lease")
	return result.LeaseUntil, err
}

// Delegate 提交当前运行的子任务请求，并返回 Hub 的原始 JSON 回执。
func (client *Client) Delegate(ctx context.Context, id Identity, run string, input Delegation) (json.RawMessage, error) {
	var result json.RawMessage
	err := client.collaboration(ctx, id, http.MethodPost, input, &result, "runs", run, "children")
	return result, err
}

// ValidateWait 向 Hub 预检当前运行提交的等待条件。
func (client *Client) ValidateWait(ctx context.Context, id Identity, run string, event RunEvent) error {
	return client.collaboration(ctx, id, http.MethodPost, event, nil, "runs", run, "wait", "validate")
}

// CreateTransfer 为当前运行登记投递元数据，并返回 Hub 的投递记录。
func (client *Client) CreateTransfer(ctx context.Context, id Identity, run string, input Manifest) (Transfer, error) {
	var result Transfer
	err := client.collaboration(ctx, id, http.MethodPost, input, &result, "runs", run, "transfers")
	return result, err
}

// Outgoing 读取 Hub 上由本设备发送的投递列表。
func (client *Client) Outgoing(ctx context.Context, id Identity) ([]Transfer, error) {
	var payload json.RawMessage
	err := client.collaboration(ctx, id, http.MethodGet, nil, &payload, "transfers", "outgoing")
	if err != nil {
		return nil, err
	}
	var result []Transfer
	if len(payload) > 0 && payload[0] == '[' {
		err = json.Unmarshal(payload, &result)
		return result, err
	}
	var wrapped struct {
		Transfers []Transfer `json:"transfers"`
	}
	err = json.Unmarshal(payload, &wrapped)
	return wrapped.Transfers, err
}

// VerifyTransfer 请 Hub 校验接收设备提交的投递票据，并返回待校验的投递元数据。
func (client *Client) VerifyTransfer(ctx context.Context, id Identity, transfer, ticket string) (Transfer, error) {
	var result Transfer
	err := client.collaboration(ctx, id, http.MethodPost, map[string]string{"ticket": ticket}, &result, "transfers", transfer, "verify")
	return result, err
}

// AckTransfer 使用投递票据向 Hub 报告接收完成。
func (client *Client) AckTransfer(ctx context.Context, id Identity, transfer, ticket string) error {
	return client.collaboration(ctx, id, http.MethodPut, map[string]string{"ticket": ticket}, nil, "transfers", transfer, "ack")
}

// ReportTransferError 向 Hub 写入投递的最近失败原因。
func (client *Client) ReportTransferError(ctx context.Context, id Identity, transfer, reason string) error {
	return client.collaboration(ctx, id, http.MethodPut, map[string]string{"reason": reason}, nil, "transfers", transfer, "error")
}

// AuthorizeTransfer 向 Hub 申请或刷新已登记投递的发送票据。
func (client *Client) AuthorizeTransfer(ctx context.Context, id Identity, transfer string) (Transfer, error) {
	var result Transfer
	err := client.collaboration(ctx, id, http.MethodPost, nil, &result, "transfers", transfer, "authorize")
	return result, err
}

func (client *Client) collaboration(ctx context.Context, id Identity, method string, input, output any, parts ...string) error {
	endpoint, err := client.deviceURL(id, parts...)
	if err != nil {
		return err
	}
	response, err := client.send(ctx, method, endpoint, id.Token, input, http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if output == nil {
		return nil
	}
	const limit = 512 << 10
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(data) > limit {
		return errors.New("Hub 协作响应无法读取")
	}
	return json.Unmarshal(data, output)
}
