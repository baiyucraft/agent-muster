// Package inputreceipt 定义设备与 Hub 共用的输入处理回执。
package inputreceipt

import (
	"strings"
	"unicode/utf8"
)

// Header 是设备领取时声明支持输入回执的请求头。值 1 表示支持。
const Header = "Muster-Input-Receipt"

// MaxClaimBytes 是当前和旧版设备允许读取的整个运行领取响应的字节上限。
const MaxClaimBytes = 256 << 10

// MaxReceiptBytes 限制全部输入 ID 的 JSON 回执体积，为终态正文和其他证据保留空间。
const MaxReceiptBytes = 16 << 10

// Receipt 列出本轮明确处理的输入 ID。空列表表示未确认任何输入。
type Receipt struct {
	HandledIDs []string `json:"handled_input_ids"`
}

// Valid 检查回执是否有明确的列表且输入 ID 格式有效、无重复。
// nil 表示当前事件没有提交回执。
func (receipt *Receipt) Valid() bool {
	if receipt == nil {
		return true
	}
	if receipt.HandledIDs == nil {
		return false
	}
	seen := make(map[string]bool, len(receipt.HandledIDs))
	for _, id := range receipt.HandledIDs {
		kind, source, found := strings.Cut(id, ":")
		if !found || (kind != "message" && kind != "transfer") ||
			source == "" || len(source) > 128 || !utf8.ValidString(source) || seen[id] {
			return false
		}
		for _, r := range source {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
				r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return false
			}
		}
		seen[id] = true
	}
	return true
}

// ValidFor 校验回执只引用本轮冻结的输入。完成或授权推送时须确认全部输入。
func (receipt *Receipt) ValidFor(expected []string, requireAll bool) bool {
	if receipt == nil {
		return !requireAll || len(expected) == 0
	}
	if !receipt.Valid() || requireAll && len(receipt.HandledIDs) != len(expected) {
		return false
	}
	allowed := make(map[string]bool, len(expected))
	for _, id := range expected {
		allowed[id] = true
	}
	for _, id := range receipt.HandledIDs {
		if !allowed[id] {
			return false
		}
	}
	return true
}
