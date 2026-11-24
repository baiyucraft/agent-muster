//go:build !windows

package runner

import "errors"

// HandleProcessProofHelper 在不支持设备进程执行的平台上忽略辅助进程参数。
func HandleProcessProofHelper([]string) (bool, error) {
	return false, nil
}

// ProcessExitConfirmed 返回当前平台无法核实 Windows 进程退出的错误。
func ProcessExitConfirmed(string) (bool, error) {
	return false, errors.New("进程退出核对目前支持 Windows")
}
