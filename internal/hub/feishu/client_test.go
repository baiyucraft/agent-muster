package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/im"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

const (
	testApprovalID = "approval-a5898923-aea5-3f62-9d1f-7634cf0f0e12"
	testMessageID  = "om_approval"
	testAppID      = "cli_muster_fixture"
	testOpenID     = "ou_admin"
)

func TestSendAndUpdateOriginalMessage(t *testing.T) {
	type request struct {
		ReceiveID   string `json:"receive_id"`
		MessageType string `json:"msg_type"`
		UUID        string `json:"uuid"`
		Content     string `json:"content"`
	}
	creates := make(chan request, 3)
	patches := make(chan request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"fixture_token","expire":7200}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture_token" {
			t.Errorf("请求缺少租户令牌")
		}
		var payload request
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解码请求失败：%v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open-apis/im/v1/messages" && r.URL.Query().Get("receive_id_type") == "open_id":
			creates <- payload
			if len(creates) == 3 {
				_, _ = io.WriteString(w, `{"code":547001,"msg":"sensitive body","data":null}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"`+testMessageID+`"}}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/open-apis/im/v1/messages/"+testMessageID:
			patches <- payload
			_, _ = io.WriteString(w, `{"code":0}`)
		default:
			t.Errorf("请求方法或路径不符：%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := newClient(server.URL, testAppID, "fixture_secret")
	a := workspace.Approval{ID: testApprovalID, Status: "pending", Question: "继续运行？"}
	for range 2 {
		id, err := client.Send(t.Context(), testOpenID, testApprovalID, a)
		if err != nil || id != testMessageID {
			t.Fatalf("发送回执 = %q, %v", id, err)
		}
	}
	if id, err := client.Send(t.Context(), testOpenID, testApprovalID, a); id != "" || err == nil || strings.Contains(err.Error(), "sensitive body") {
		t.Fatalf("渠道拒绝应返回脱敏错误且不确认发送，得到 %q, %v", id, err)
	}
	for range 3 {
		select {
		case payload := <-creates:
			if payload.UUID != testApprovalID || payload.ReceiveID != testOpenID || payload.MessageType != "interactive" {
				t.Fatalf("私聊发送字段不符：%+v", payload)
			}
		default:
			t.Fatal("缺少发送请求")
		}
	}
	a.Status, a.Answer = "answered", "继续"
	for range 2 {
		if err := client.Update(t.Context(), testMessageID, a); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		select {
		case payload := <-patches:
			if strings.Contains(payload.Content, submitPrefix) || !strings.Contains(payload.Content, "已答复") {
				t.Fatalf("原卡片更新未反映已答复状态：%+v", payload)
			}
		default:
			t.Fatal("缺少原消息更新请求")
		}
	}
}

func TestCardActionDispatchRoundTrip(t *testing.T) {
	client := New(testAppID, "fixture_secret")
	const literal = "[链接](https://example.invalid) <at id=all>"
	encoded, err := renderCard(workspace.Approval{ID: testApprovalID, Question: literal, Options: []string{"继续"}, Status: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	var card struct {
		Body struct {
			Elements []struct {
				Tag       string
				Text      struct{ Tag, Content string }
				Behaviors []struct{ Value map[string]any }
				Elements  []struct{ Tag, Name string }
			}
		}
	}
	if err := json.Unmarshal([]byte(encoded), &card); err != nil {
		t.Fatal(err)
	}
	question, option, form := card.Body.Elements[0], card.Body.Elements[1], card.Body.Elements[2]
	if question.Text.Content != literal || question.Text.Tag != "plain_text" {
		t.Fatal("问题未按纯文本渲染")
	}
	submit := form.Elements[1]
	optionAction := map[string]any{"tag": option.Tag, "value": option.Behaviors[0].Value}
	formAction := map[string]any{"tag": submit.Tag, "name": submit.Name, "form_value": map[string]string{form.Elements[0].Name: "  已确认  "}}

	var replies []im.Reply
	var callbackError error
	handler := client.eventHandler(func(ctx context.Context, reply im.Reply) error {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > replyTimeout {
			t.Fatal("回调缺少保存期限")
		}
		replies = append(replies, reply)
		return callbackError
	})
	for _, action := range []map[string]any{optionAction, formAction} {
		body := cardEvent(t, testAppID, testMessageID, action)
		result, err := handler.Do(t.Context(), body)
		response, ok := result.(*callback.CardActionTriggerResponse)
		if err != nil || !ok || response == nil || response.Toast == nil || response.Toast.Type != "success" || response.Card != nil {
			t.Fatalf("回调确认 = %+v, %v", result, err)
		}
	}
	if len(replies) != 2 {
		t.Fatalf("答复次数 = %d", len(replies))
	}
	for _, reply := range replies {
		if reply.ApprovalID != testApprovalID || reply.OpenID != testOpenID || reply.MessageID != testMessageID {
			t.Fatalf("答复身份不符：%+v", reply)
		}
	}
	if replies[0].Option == nil || *replies[0].Option != 0 || replies[0].Text != "" || replies[1].Option != nil || replies[1].Text != "已确认" {
		t.Fatalf("答复内容不符：%+v", replies)
	}
	for _, body := range [][]byte{
		cardEvent(t, "other_app", testMessageID, optionAction),
		cardEvent(t, "", testMessageID, formAction),
		cardEvent(t, testAppID, "", optionAction),
		cardEvent(t, testAppID, testMessageID, map[string]any{"tag": option.Tag, "value": map[string]any{"approval_id": testApprovalID, "option": 0.5}}),
		cardEvent(t, testAppID, testMessageID, map[string]any{"tag": submit.Tag, "name": submit.Name, "form_value": map[string]any{answerField: 42}}),
	} {
		if _, err := handler.Do(t.Context(), body); err != nil {
			t.Fatal(err)
		}
	}
	if len(replies) != 2 {
		t.Fatalf("无效回调触发保存：%+v", replies[2:])
	}
	callbackError = im.ErrReplyUnavailable
	result, err := handler.Do(t.Context(), cardEvent(t, testAppID, testMessageID, optionAction))
	response, ok := result.(*callback.CardActionTriggerResponse)
	if err != nil || !ok || response == nil || response.Toast == nil || response.Toast.Type != "warning" {
		t.Fatalf("无法关联的回调应返回提示，得到 %+v, %v", result, err)
	}
	callbackError = errors.New("sensitive database error")
	_, err = handler.Do(t.Context(), cardEvent(t, testAppID, testMessageID, optionAction))
	if !errors.Is(err, errReplyFailed) {
		t.Fatalf("数据库失败应返回可重传的脱敏错误，得到 %v", err)
	}
}

func TestDirectTextReplyRequiresDirectCardReference(t *testing.T) {
	client := New(testAppID, "fixture_secret")
	var replies []im.Reply
	handler := client.eventHandler(func(_ context.Context, reply im.Reply) error {
		replies = append(replies, reply)
		return nil
	})
	for _, input := range []struct{ app, chat, sender, parent string }{
		{testAppID, "p2p", "user", testMessageID},
		{"other_app", "p2p", "user", testMessageID},
		{testAppID, "group", "user", testMessageID},
		{testAppID, "p2p", "bot", testMessageID},
		{testAppID, "p2p", "user", ""},
	} {
		body := eventJSON(t, "im.message.receive_v1", input.app, map[string]any{
			"sender":  map[string]any{"sender_type": input.sender, "sender_id": map[string]string{"open_id": testOpenID}},
			"message": map[string]string{"chat_type": input.chat, "message_type": "text", "root_id": testMessageID, "parent_id": input.parent, "content": `{"text":"  继续  "}`},
		})
		if _, err := handler.Do(t.Context(), body); err != nil {
			t.Fatal(err)
		}
	}
	if len(replies) != 1 || replies[0] != (im.Reply{OpenID: testOpenID, MessageID: testMessageID, Text: "继续"}) {
		t.Fatalf("私聊回复解析不符：%+v", replies)
	}
}

func TestListenCancellationDuringBootstrap(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- newClient(server.URL, testAppID, "fixture_secret").Listen(ctx, func(context.Context, im.Reply) error { return nil })
	}()
	select {
	case <-started:
		cancel()
	case <-ctx.Done():
		t.Fatal("长连接未发起握手")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消长连接得到 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("长连接未响应取消")
	}
}

func cardEvent(t *testing.T, appID, messageID string, action map[string]any) []byte {
	t.Helper()
	return eventJSON(t, "card.action.trigger", appID, map[string]any{
		"context":  map[string]string{"open_message_id": messageID},
		"operator": map[string]string{"open_id": testOpenID},
		"host":     "im_message", "action": action,
	})
}

func eventJSON(t *testing.T, eventType, appID string, data map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"schema": "2.0", "header": map[string]string{"app_id": appID, "event_type": eventType}, "event": data,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
