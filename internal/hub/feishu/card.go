package feishu

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

const (
	answerField  = "answer"
	submitPrefix = "submit_answer_"
	answerBytes  = 2000
)

// renderCard 使用纯文本展示所有业务内容。仅待确认卡片包含答复操作。
func renderCard(a workspace.Approval) (string, error) {
	var title, theme string
	switch a.Status {
	case "pending":
		title, theme = "待答复", "blue"
	case "answered":
		title, theme = "已答复", "green"
	case "handled":
		title, theme = "已处理", "grey"
	default:
		return "", errors.New("飞书请示的状态无效")
	}
	plain := func(content string) map[string]string {
		return map[string]string{"tag": "plain_text", "content": content}
	}
	text := func(content string) map[string]any {
		return map[string]any{"tag": "div", "text": plain(content)}
	}
	elements := []map[string]any{text(a.Question)}
	if a.Impact != "" {
		elements = append(elements, text(a.Impact))
	}
	if a.Status == "pending" {
		for index, label := range a.Options {
			elements = append(elements, map[string]any{
				"tag": "interactive_container", "width": "fill", "has_border": true,
				"behaviors": []map[string]any{{"type": "callback", "value": map[string]string{
					"approval_id": a.ID, "option": strconv.Itoa(index),
				}}},
				"elements": []map[string]any{text(label)},
			})
		}
		elements = append(elements, map[string]any{
			"tag": "form", "name": "approval_answer", "elements": []map[string]any{
				{
					"tag": "input", "name": answerField, "input_type": "multiline_text",
					"rows": 2, "max_length": 1000, "required": true, "placeholder": plain("填写答复…"),
				},
				{
					// 表单提交按钮用组件名称携带请示 ID，名称只包含字母、数字和下划线。
					"tag": "button", "name": submitPrefix + strings.ReplaceAll(a.ID, "-", "_"), "form_action_type": "submit",
					"type": "primary", "text": plain("提交答复"),
				},
			},
		})
	} else if a.Answer != "" {
		elements = append(elements, text(a.Answer))
	}
	card := map[string]any{
		"schema": "2.0", "config": map[string]bool{"update_multi": true, "enable_forward": false},
		"header": map[string]any{"title": plain("Agent Muster · " + title), "template": theme},
		"body":   map[string]any{"elements": elements},
	}
	encoded, err := json.Marshal(card)
	if err != nil {
		return "", errors.New("飞书请示卡片编码失败")
	}
	return string(encoded), nil
}
