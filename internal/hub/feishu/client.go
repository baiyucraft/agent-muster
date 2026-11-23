// Package feishu 将 Hub 的请示交给飞书机器人私聊，并接收已绑定消息的答复。
package feishu

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/im"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	baseURL        = "https://open.feishu.cn"
	requestTimeout = 8 * time.Second
)

var (
	_                im.Transport = (*Client)(nil)
	messageIDPattern              = regexp.MustCompile(`^om_[A-Za-z0-9_-]{1,128}$`)
)

// Client 使用自建应用凭据发送、更新卡片，以及接收长连接答复。
type Client struct {
	config *larkcore.Config
	api    *larkim.V1
}

// New 配置飞书客户端。创建时无网络请求，SDK 在首次发送时获取租户令牌。
func New(appID, appSecret string) *Client {
	return newClient(baseURL, appID, appSecret)
}

func newClient(url, appID, appSecret string) *Client {
	config := &larkcore.Config{
		BaseUrl: url, AppId: appID, AppSecret: appSecret,
		AppType: larkcore.AppTypeSelfBuilt, EnableTokenCache: true,
		Logger: discardLogger{}, Serializable: &larkcore.DefaultSerialization{},
		HttpClient: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	return &Client{config: config, api: larkim.New(config)}
}

// Send 发送机器人私聊卡片并返回飞书的原消息 ID。重试要沿用同一个 key，飞书对此去重一小时。
func (c *Client) Send(ctx context.Context, openID, key string, a workspace.Approval) (string, error) {
	if openID == "" || key == "" || len(key) > 50 {
		return "", errors.New("飞书请示缺少收件人或有效的去重键")
	}
	content, err := renderCard(a)
	if err != nil {
		return "", err
	}
	request := larkim.NewCreateMessageReqBuilder().ReceiveIdType("open_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(openID).
			MsgType("interactive").Content(content).Uuid(key).Build()).Build()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	resp, err := c.api.Message.Create(ctx, request)
	if err != nil {
		return "", requestError(ctx, "发送飞书请示", err)
	}
	if resp.StatusCode != http.StatusOK || !resp.Success() {
		return "", fmt.Errorf("发送飞书请示失败（HTTP %d，飞书错误码 %d）", resp.StatusCode, resp.Code)
	}
	if resp.Data == nil || !messageIDPattern.MatchString(value(resp.Data.MessageId)) {
		return "", errors.New("发送飞书请示后缺少有效的原消息 ID")
	}
	return *resp.Data.MessageId, nil
}

// Update 将答复状态写入原消息。重复更新同一快照仍指向同一张卡片。
func (c *Client) Update(ctx context.Context, messageID string, a workspace.Approval) error {
	if !messageIDPattern.MatchString(messageID) {
		return errors.New("更新飞书请示需要有效的原消息 ID")
	}
	content, err := renderCard(a)
	if err != nil {
		return err
	}
	request := larkim.NewPatchMessageReqBuilder().MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(content).Build()).Build()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	resp, err := c.api.Message.Patch(ctx, request)
	if err != nil {
		return requestError(ctx, "更新飞书请示", err)
	}
	if resp.StatusCode != http.StatusOK || !resp.Success() {
		return fmt.Errorf("更新飞书请示失败（HTTP %d，飞书错误码 %d）", resp.StatusCode, resp.Code)
	}
	return nil
}

// SDK 返回的底层错误可能包含响应正文或长连接凭据，只保留可安全记录的信息。
func requestError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s失败：%w", operation, ctx.Err())
	}
	var code *larkcore.CodeError
	if errors.As(err, &code) {
		return fmt.Errorf("%s失败（飞书错误码 %d）", operation, code.Code)
	}
	return fmt.Errorf("%s的渠道请求失败", operation)
}

func value(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}

// SDK 日志可包含原始飞书消息，调用方通过脱敏错误记录失败。
type discardLogger struct{}

// Debug 丢弃 SDK 调试信息，避免记录请求正文。
func (discardLogger) Debug(context.Context, ...any) {}

// Info 丢弃 SDK 连接信息，避免记录临时连接地址。
func (discardLogger) Info(context.Context, ...any) {}

// Warn 丢弃 SDK 警告，调用方记录脱敏后的错误。
func (discardLogger) Warn(context.Context, ...any) {}

// Error 丢弃 SDK 原始错误，避免记录凭据或消息内容。
func (discardLogger) Error(context.Context, ...any) {}
