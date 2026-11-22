package main

import (
	"errors"
	"fmt"
	"log"

	"golang.org/x/sys/windows"
)

// claimDesktopInstance 创建当前用户、会话和 profile 的唤起事件。
// 返回零句柄时，已有实例仍持有事件，普通启动已向它请求打开窗口。
func claimDesktopInstance(profile string, open bool) (windows.Handle, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return 0, fmt.Errorf("读取当前 Windows 用户: %w", err)
	}
	name, err := windows.UTF16PtrFromString(`Local\AgentMuster.Device.` + user.User.Sid.String() + "." + profile)
	if err != nil {
		return 0, err
	}
	signal, err := windows.CreateEvent(nil, 0, 0, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		defer windows.CloseHandle(signal)
		if open {
			return 0, windows.SetEvent(signal)
		}
		return 0, nil
	}
	return signal, err
}

// watchDesktopOpen 处理启动期间和常驻期间的窗口唤起请求。关闭时先停等候，再释放句柄。
func watchDesktopOpen(signal windows.Handle, open func()) (func(), error) {
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			which, err := windows.WaitForMultipleObjects([]windows.Handle{stop, signal}, false, windows.INFINITE)
			if err != nil {
				log.Printf("等候设备窗口唤起失败: %v", err)
				return
			}
			if which != windows.WAIT_OBJECT_0+1 {
				return
			}
			open()
		}
	}()
	return func() {
		_ = windows.SetEvent(stop)
		<-done
		_ = windows.CloseHandle(stop)
	}, nil
}
