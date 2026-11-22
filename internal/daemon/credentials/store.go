// Package credentials 在当前用户的 Windows 凭据管理器中保存设备身份。
package credentials

import (
	"errors"
	"regexp"
)

var (
	ErrNotEnrolled = errors.New("当前配置尚未配对，请运行 muster-daemon enroll")
	profilePattern = regexp.MustCompile("^[a-z0-9][a-z0-9-]{0,47}$")
)

// Store 持有一个配对配置在系统凭据管理器中的目标名。
type Store struct {
	target string
}

// New 校验配对配置名，并生成该配置对应的凭据目标。
func New(profile string) (Store, error) {
	if !profilePattern.MatchString(profile) {
		return Store{}, errors.New("-profile 须为 1 到 48 个小写字母、数字或短横线，并以字母或数字开头")
	}
	return Store{target: "AgentMuster/" + profile}, nil
}
