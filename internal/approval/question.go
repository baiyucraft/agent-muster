// Package approval 定义设备和 Hub 共用的请示内容与校验规则。
package approval

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Question 是设备在本轮退出后提交的请示。选项为快捷答复，也可提交自由文本。
type Question struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Impact   string   `json:"impact,omitempty"`
}

// Normalize 返回去除首尾空白的请示副本。缺少选项时使用空数组，所有长度限制以 UTF-8 字节计。
func (q Question) Normalize() (Question, error) {
	if !validText(q.Question, 2000) || !validText(q.Impact, 2000) || strings.TrimSpace(q.Question) == "" {
		return Question{}, errors.New("请示问题不能为空，问题和影响范围各限 2000 个 UTF-8 字节")
	}
	if len(q.Options) > 8 {
		return Question{}, errors.New("请示最多包含 8 个选项")
	}
	value := Question{Question: strings.TrimSpace(q.Question), Impact: strings.TrimSpace(q.Impact), Options: make([]string, 0, len(q.Options))}
	seen := make(map[string]bool, len(q.Options))
	for _, option := range q.Options {
		label := strings.TrimSpace(option)
		if !validText(option, 240) || label == "" || seen[label] {
			return Question{}, errors.New("请示选项须唯一且非空，每项限 240 个 UTF-8 字节")
		}
		seen[label] = true
		value.Options = append(value.Options, label)
	}
	return value, nil
}

func validText(value string, limit int) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0) && len(value) <= limit
}
