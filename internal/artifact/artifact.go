// Package artifact 定义设备本地文件快照的引用及读取协议。
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"strings"
	"unicode"
)

const (
	// MaxCount 限制一轮执行可登记的文件版本数。
	MaxCount = 8
	// MaxSize 是单份本地快照和下载正文的字节上限。
	MaxSize = 32 << 20
	// MaxPath 是绑定目录相对路径的 UTF-8 字节上限。
	MaxPath = 512
)

// ErrMissing 表示设备无法找到已经登记的快照。
var ErrMissing = errors.New("artifact snapshot missing")

// Reference 是一份本地快照的不可变元数据。Path 相对于本轮工作目录。
type Reference struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ReadRequest 由 Hub 发给产物所属的设备，仅在管理员读取期间有效。
type ReadRequest struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	ArtifactID string `json:"artifact_id"`
}

// ID 将文件路径和内容版本绑定到产生它的运行。
func ID(runID, path, digest string) string {
	reference := sha256.Sum256([]byte(runID + "\x00" + path + "\x00" + digest))
	return hex.EncodeToString(reference[:])
}

// ValidID 校验 SHA-256 及以摘要标识的产物 ID。
func ValidID(id string) bool {
	return len(id) == 64 && lowercaseHex(id)
}

func lowercaseHex(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
	}) == -1
}

// ValidReadID 校验 Hub 为单次临时读取生成的随机标识。
func ValidReadID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "artifact-")
	return ok && len(suffix) == 32 && lowercaseHex(suffix)
}

// ValidPath 校验采用斜杠分隔的 Windows 相对文件路径。
func ValidPath(path string) bool {
	if path == "." || len(path) > MaxPath || !fs.ValidPath(path) || strings.ContainsAny(path, `\:*?"<>|`) ||
		strings.ContainsFunc(path, unicode.IsControl) {
		return false
	}
	for segment := range strings.SplitSeq(path, "/") {
		if strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
			return false
		}
	}
	return true
}

// ValidFor 校验产物数量、大小、唯一标识及其运行归属。
func ValidFor(runID string, refs []Reference) bool {
	if len(refs) > MaxCount {
		return false
	}
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if ref.Size < 0 || ref.Size > MaxSize || !ValidPath(ref.Path) || !ValidID(ref.SHA256) ||
			ref.ID != ID(runID, ref.Path, ref.SHA256) || seen[ref.ID] {
			return false
		}
		seen[ref.ID] = true
	}
	return true
}

// Matches 校验读取正文与登记元数据中的大小和 SHA-256 一致。
func (ref Reference) Matches(body []byte) bool {
	if int64(len(body)) != ref.Size {
		return false
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]) == ref.SHA256
}
