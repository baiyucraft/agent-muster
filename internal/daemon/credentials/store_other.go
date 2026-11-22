//go:build !windows

package credentials

import "errors"

var errUnsupported = errors.New("设备凭据存储当前支持 Windows")

// Load 在非 Windows 平台返回凭据存储不可用错误。
func (Store) Load() ([]byte, error) {
	return nil, errUnsupported
}

// Save 在非 Windows 平台返回凭据存储不可用错误。
func (Store) Save([]byte) error {
	return errUnsupported
}

// DuringEnrollment 在非 Windows 平台返回凭据存储不可用错误。
func (Store) DuringEnrollment(func() error) error {
	return errUnsupported
}
