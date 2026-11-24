// Package adminauth 定义 Hub 单管理员的凭据格式和密码校验。
package adminauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	// MaxPasswordBytes 是管理员密码的 UTF-8 字节上限。
	MaxPasswordBytes = 1024
	saltBytes        = 16
	digestBytes      = 32
	memoryKiB        = 19 * 1024
	iterations       = 2
	parallelism      = 1
	hashPrefix       = "$argon2id$v=19$m=19456,t=2,p=1$"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ValidUsername 检查区分大小写的账号名是否符合长度及字符限制。
func ValidUsername(username string) bool {
	return usernamePattern.MatchString(username)
}

// ValidPassword 检查密码是否为 12 至 1024 字节的有效 UTF-8 文本。
func ValidPassword(password string) bool {
	return len(password) >= 12 && len(password) <= MaxPasswordBytes && utf8.ValidString(password)
}

// HashPassword 生成带随机盐的 Argon2id 哈希，供初始化或重置管理员使用。
func HashPassword(password string) (string, error) {
	if !ValidPassword(password) {
		return "", errors.New("管理员密码须为 12 至 1024 字节的 UTF-8 文本")
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest := argon2.IDKey([]byte(password), salt, iterations, memoryKiB, parallelism, digestBytes)
	return hashPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(digest), nil
}

// VerifyPassword 使用固定的 Argon2id 参数验证哈希。格式错误时拒绝验证。
func VerifyPassword(hash, password string) bool {
	encoded, ok := strings.CutPrefix(hash, hashPrefix)
	if !ok || !ValidPassword(password) {
		return false
	}
	encodedSalt, encodedDigest, ok := strings.Cut(encoded, "$")
	if !ok || len(encodedSalt) != base64.RawStdEncoding.EncodedLen(saltBytes) ||
		len(encodedDigest) != base64.RawStdEncoding.EncodedLen(digestBytes) {
		return false
	}
	salt, saltErr := base64.RawStdEncoding.Strict().DecodeString(encodedSalt)
	digest, digestErr := base64.RawStdEncoding.Strict().DecodeString(encodedDigest)
	if saltErr != nil || digestErr != nil {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memoryKiB, parallelism, digestBytes)
	return subtle.ConstantTimeCompare(actual, digest) == 1
}
