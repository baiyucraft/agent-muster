package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/baiyucraft/agent-muster/internal/daemon/credentials"
	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const loginStartupKey = "Software\\Microsoft\\Windows\\CurrentVersion\\Run"

// addStartupMenu 将托盘勾选状态与当前配置的启动项同步。写入失败时还原勾选并展示错误。
func addStartupMenu(app *application.App, menu *application.Menu, profile string) {
	enabled, err := loginStartupEnabled(profile)
	if err != nil {
		log.Print(err)
	}
	item := menu.AddCheckbox("登录时静默启动", enabled)
	item.OnClick(func(_ *application.Context) {
		// Wails 先在主线程切换勾选，再异步调回。按最后一次持久化值切换，
		// 并在主线程串行更新，避免快速点击时旧回调覆盖新状态。
		err := application.InvokeSyncWithError(func() error {
			err := setLoginStartup(profile, !enabled)
			if err == nil {
				enabled = !enabled
			}
			item.SetChecked(enabled)
			return err
		})
		if err != nil {
			app.Dialog.Error().SetTitle("无法更改登录启动设置").SetMessage(err.Error()).Show()
		}
	})
}

// loginStartupEnabled 读取当前用户对指定设备配置的登录启动设置，未登记时返回 false。
func loginStartupEnabled(profile string) (bool, error) {
	name, err := loginStartupValueName(profile)
	if err != nil {
		return false, err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, loginStartupKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取登录启动设置: %w", err)
	}
	defer key.Close()

	_, _, err = key.GetValue(name, nil)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取配置的登录启动项: %w", err)
	}
	return true, nil
}

// setLoginStartup 更新当前用户指定配置的启动项。关闭可重复执行，且只删除本配置的值。
func setLoginStartup(profile string, enabled bool) error {
	name, err := loginStartupValueName(profile)
	if err != nil {
		return err
	}

	if !enabled {
		key, err := registry.OpenKey(registry.CURRENT_USER, loginStartupKey, registry.SET_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("打开登录启动设置: %w", err)
		}
		defer key.Close()
		if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("关闭登录时启动: %w", err)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("查找当前设备程序: %w", err)
	}
	command, err := loginStartupCommand(exe, profile)
	if err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, loginStartupKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开登录启动设置: %w", err)
	}
	defer key.Close()
	if err := key.SetStringValue(name, command); err != nil {
		return fmt.Errorf("启用登录时启动: %w", err)
	}
	return nil
}

func loginStartupValueName(profile string) (string, error) {
	if _, err := credentials.New(profile); err != nil {
		return "", err
	}
	return "AgentMuster-" + profile, nil
}

// loginStartupCommand 构造 Windows 直接启动所用的命令行，调用方须先验证配置名。
func loginStartupCommand(exe, profile string) (string, error) {
	if !filepath.IsAbs(exe) || strings.ContainsRune(exe, '"') {
		return "", errors.New("设备程序启动路径无效")
	}
	command := windows.ComposeCommandLine([]string{exe, "--background", "--profile=" + profile})
	encoded, err := windows.UTF16FromString(command)
	if err != nil {
		return "", fmt.Errorf("设备程序启动路径无效: %w", err)
	}
	// Windows Run 注册表值中的命令行最多 260 个字符，编码结果包含末尾空字符。
	if len(encoded) > 261 {
		return "", errors.New("登录启动命令超过 Windows 的 260 字符限制")
	}
	return command, nil
}
