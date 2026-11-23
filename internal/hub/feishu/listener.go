package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/im"
	"github.com/google/uuid"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
)

// 飞书要求在三秒内确认回调，为解析和写回留出时间。
const replyTimeout = 2 * time.Second

var errReplyFailed = errors.New("保存飞书请示答复失败")

// Listen 从飞书长连接接收卡片操作及私聊引用回复。SDK 管理重连，并在 ctx 结束后回收连接。
// onReply 在当前回调时限内提交答复，只有提交成功才确认。多个回调可能并发到达。
func (c *Client) Listen(ctx context.Context, onReply func(context.Context, im.Reply) error) error {
	listener := ws.NewClient(c.config.AppId, c.config.AppSecret,
		ws.WithDomain(c.config.BaseUrl), ws.WithHttpClient(c.config.HttpClient),
		ws.WithLogger(discardLogger{}), ws.WithWriteTimeout(replyTimeout),
		ws.WithEventHandler(c.eventHandler(onReply)))
	err := listener.Start(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var clientErr *ws.ClientError
	if errors.As(err, &clientErr) {
		return fmt.Errorf("飞书长连接关闭（飞书错误码 %d）", clientErr.Code)
	}
	return errors.New("飞书长连接关闭")
}

func (c *Client) eventHandler(onReply func(context.Context, im.Reply) error) *dispatcher.EventDispatcher {
	handler := dispatcher.NewEventDispatcher("", "")
	handler.Config.Logger = discardLogger{}
	return handler.OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		reply, ok := c.cardReply(event)
		if !ok {
			return nil, nil
		}
		err := submitReply(ctx, reply, onReply)
		if errors.Is(err, im.ErrReplyUnavailable) {
			return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "warning", Content: "无法答复此请示，请在 Web 查看"}}, nil
		}
		if err != nil {
			return nil, err
		}
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "success", Content: "请示已记录"}}, nil
	}).OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		reply, ok := c.textReply(event)
		if !ok {
			return nil
		}
		err := submitReply(ctx, reply, onReply)
		if errors.Is(err, im.ErrReplyUnavailable) {
			return nil
		}
		return err
	})
}

func submitReply(ctx context.Context, reply im.Reply, onReply func(context.Context, im.Reply) error) error {
	ctx, cancel := context.WithTimeout(ctx, replyTimeout)
	defer cancel()
	err := onReply(ctx, reply)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, im.ErrReplyUnavailable):
		return im.ErrReplyUnavailable
	default:
		return errReplyFailed
	}
}

// validApp 校验飞书回调信封中的应用 ID。用于更新卡片的 Token 不进入业务层。
func (c *Client) validApp(base *larkevent.EventV2Base) bool {
	return base != nil && base.Header != nil && base.Schema == "2.0" && c.config.AppId != "" && base.Header.AppID == c.config.AppId
}

func (c *Client) cardReply(event *callback.CardActionTriggerEvent) (im.Reply, bool) {
	if event == nil || !c.validApp(event.EventV2Base) || event.Event == nil {
		return im.Reply{}, false
	}
	data := event.Event
	if data.Operator == nil || data.Operator.OpenID == "" || data.Context == nil || data.Action == nil ||
		!messageIDPattern.MatchString(data.Context.OpenMessageID) || (data.Host != "" && data.Host != "im_message") {
		return im.Reply{}, false
	}
	reply := im.Reply{OpenID: data.Operator.OpenID, MessageID: data.Context.OpenMessageID}
	action := data.Action
	if name, found := strings.CutPrefix(action.Name, submitPrefix); action.Tag == "button" && found {
		reply.ApprovalID = strings.ReplaceAll(name, "_", "-")
		if len(action.Value) != 0 || !validApprovalID(reply.ApprovalID) {
			return im.Reply{}, false
		}
		text, _ := action.FormValue[answerField].(string)
		reply.Text = strings.TrimSpace(text)
		return reply, validAnswer(reply.Text)
	}
	if action.Tag != "interactive_container" || len(action.FormValue) != 0 {
		return im.Reply{}, false
	}
	approvalID, _ := action.Value["approval_id"].(string)
	if !validApprovalID(approvalID) {
		return im.Reply{}, false
	}
	index, _ := action.Value["option"].(string)
	option, err := strconv.Atoi(index)
	if err != nil || option < 0 || strconv.Itoa(option) != index {
		return im.Reply{}, false
	}
	reply.ApprovalID, reply.Option = approvalID, &option
	return reply, true
}

func validApprovalID(value string) bool {
	raw, found := strings.CutPrefix(value, "approval-")
	id, err := uuid.Parse(raw)
	return found && err == nil && id.String() == raw
}

func (c *Client) textReply(event *larkim.P2MessageReceiveV1) (im.Reply, bool) {
	if event == nil || !c.validApp(event.EventV2Base) || event.Event == nil {
		return im.Reply{}, false
	}
	sender, message := event.Event.Sender, event.Event.Message
	if sender == nil || sender.SenderId == nil || value(sender.SenderType) != "user" ||
		value(sender.SenderId.OpenId) == "" || message == nil || value(message.ChatType) != "p2p" || value(message.MessageType) != "text" {
		return im.Reply{}, false
	}
	// 优先关联直接引用的卡片，避免把回复会话中其他消息误当作请示答复。
	parent := value(message.ParentId)
	if !messageIDPattern.MatchString(parent) || message.Content == nil {
		return im.Reply{}, false
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(*message.Content), &content); err != nil {
		return im.Reply{}, false
	}
	text := strings.TrimSpace(content.Text)
	return im.Reply{OpenID: *sender.SenderId.OpenId, MessageID: parent, Text: text}, validAnswer(text)
}

func validAnswer(text string) bool {
	return text != "" && len(text) <= answerBytes && !strings.ContainsRune(text, 0)
}
