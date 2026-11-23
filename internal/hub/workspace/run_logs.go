package workspace

import "errors"

// ErrRunLogsUnavailable 表示运行尚未领取或退出，暂不接受日志读取请求。
var ErrRunLogsUnavailable = errors.New("run logs require a finished device run")

// ErrRunLogDeviceRevoked 表示本次运行的执行设备已撤销。
var ErrRunLogDeviceRevoked = errors.New("execution device was revoked")

// ErrRunLogDeviceOffline 表示运行所属设备超过了心跳有效期。
var ErrRunLogDeviceOffline = errors.New("execution device is offline")
