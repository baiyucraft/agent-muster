// Package im 驱动 Hub 的请示通知与回复，跨入口复用同一条请示。
package im

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

// ErrReplyUnavailable 表示该消息与当前渠道的请示无法匹配，或该请示已无法继续。
var ErrReplyUnavailable = errors.New("approval reply unavailable")

// Recipient 标识部署者绑定的应用和私聊账号。OpenID 的作用域为 AppID。
type Recipient struct {
	AppID  string
	OpenID string
}

// Reply 携带私聊原消息 ID、答复账号和内容。卡片操作同时携带请示 ID 和可选的选项序号。
type Reply struct {
	ApprovalID string
	OpenID     string
	MessageID  string
	Text       string
	Option     *int
}

// Delivery 是当前租约领取的卡片操作。MessageID 为空时创建卡片，否则更新原卡片。
type Delivery struct {
	Approval  workspace.Approval
	MessageID string
	Lease     string
}

// Store 持久化通知、卡片绑定和首条答复。领取空 Delivery 表示当前无待办。
type Store interface {
	ClaimApprovalNotification(context.Context, Recipient) (Delivery, error)
	CompleteApprovalNotification(context.Context, Recipient, Delivery, string) error
	RetryApprovalNotification(context.Context, Recipient, Delivery) error
	AnswerApprovalFromIM(context.Context, Recipient, Reply) (workspace.Approval, error)
}

// Transport 发送或更新私聊卡片，Listen 通过渠道长连接交付已解码的用户答复。
// Send 的 key 是渠道幂等键，Update 同一消息和状态可安全重试。
type Transport interface {
	Send(ctx context.Context, openID, key string, approval workspace.Approval) (string, error)
	Update(ctx context.Context, messageID string, approval workspace.Approval) error
	Listen(ctx context.Context, onReply func(context.Context, Reply) error) error
}

// Run 并行处理持久化通知和长连接答复，直到 ctx 取消。
// 渠道中断会继续重连，Web 和设备端使用独立的请求路径。
func Run(ctx context.Context, store Store, recipient Recipient, transport Transport) {
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		listen(ctx, store, recipient, transport)
	}()
	for ctx.Err() == nil {
		if deliver(ctx, store, recipient, transport) {
			continue
		}
		wait(ctx, time.Second)
	}
	<-stopped
}

func listen(ctx context.Context, store Store, recipient Recipient, transport Transport) {
	for ctx.Err() == nil {
		err := transport.Listen(ctx, func(callback context.Context, reply Reply) error {
			_, err := store.AnswerApprovalFromIM(callback, recipient, reply)
			return err
		})
		if ctx.Err() == nil {
			log.Printf("IM 请示长连接关闭，稍后重连 (%T)", err)
			wait(ctx, 5*time.Second)
		}
	}
}

// deliver 按领取时的状态快照发送一次。可能已送达但确认失败时，继续使用同一业务键。
func deliver(ctx context.Context, store Store, recipient Recipient, transport Transport) bool {
	request, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	delivery, err := store.ClaimApprovalNotification(request, recipient)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("读取 IM 请示通知失败 (%T)", err)
		}
		return false
	}
	if delivery.Lease == "" {
		return false
	}
	messageID := delivery.MessageID
	if messageID == "" {
		messageID, err = transport.Send(request, recipient.OpenID, delivery.Approval.ID, delivery.Approval)
		if err == nil && messageID == "" {
			err = errors.New("IM send returned empty message ID")
		}
	} else {
		err = transport.Update(request, messageID, delivery.Approval)
	}

	// 网络请求可能已经消耗完整超时。在独立的时间窗口记账，退出时也能保存渠道回执。
	save, finish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finish()
	if err != nil {
		if saveErr := store.RetryApprovalNotification(save, recipient, delivery); saveErr != nil {
			log.Printf("保存 IM 请示通知的重试计划失败 (%T)", saveErr)
		}
		if ctx.Err() == nil {
			log.Printf("投递 IM 请示通知失败 (%T)", err)
		}
		return false
	}
	if err := store.CompleteApprovalNotification(save, recipient, delivery, messageID); err != nil {
		log.Printf("保存 IM 请示通知的渠道回执失败 (%T)", err)
		return false
	}
	return true
}

func wait(ctx context.Context, interval time.Duration) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
