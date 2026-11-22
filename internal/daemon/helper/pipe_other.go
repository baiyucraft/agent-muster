//go:build !windows

package helper

import (
	"context"
	"errors"
	"net"
)

func listen() (net.Listener, error) { return nil, errors.New("设备本机命令目前支持 Windows") }
func dial(context.Context, string) (net.Conn, error) {
	return nil, errors.New("设备本机命令目前支持 Windows")
}
