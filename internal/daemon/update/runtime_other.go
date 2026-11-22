//go:build !windows

package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
)

// Stage 在非 Windows 平台返回更新暂存不受支持的错误。
func Stage(context.Context, Offer, ed25519.PublicKey) (*Prepared, error) {
	return nil, errors.New("automatic device updates are supported on Windows only")
}

// Start 在非 Windows 平台返回更新安装不受支持的错误。
func (*Prepared) Start(string, string) error {
	return errors.New("automatic device updates are supported on Windows only")
}

// LockInstallation 在非 Windows 平台返回无需释放资源的函数。
func LockInstallation(string) (func(), error) { return func() {}, nil }

// MarkHealthy 在非 Windows 平台直接返回。
func MarkHealthy(string) error { return nil }

// HandleHelper 在非 Windows 平台不处理更新器参数。
func HandleHelper([]string) (bool, error) { return false, nil }

func replaceFile(source, destination string) error { return os.Rename(source, destination) }

func openRecord(path string) (*os.File, error) { return os.Open(path) }
