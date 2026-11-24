//go:build windows

package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"os"
	"sync/atomic"

	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"github.com/baiyucraft/agent-muster/internal/daemon/update"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"golang.org/x/sys/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/windows/icon.ico
var trayIcon []byte

func main() {
	// 守护进程等待本轮 Job 结束，无需打开桌面或取得工作区锁。
	if handled, err := runner.HandleProcessProofHelper(os.Args[1:]); handled {
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	// 更新辅助进程由同一程序启动，处理替换请求时无需初始化桌面窗口。
	if handled, err := update.HandleHelper(os.Args[1:]); handled {
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	profile := flag.String("profile", "default", "本机设备配置")
	background := flag.Bool("background", false, "启动到系统托盘")
	health := flag.String("update-health", "", "")
	flag.Parse()
	// 先取得安装锁，避免更新替换期间的普通启动占用试运行的唤起事件。
	unlock, err := update.LockInstallation(*health)
	if err != nil {
		log.Fatal(err)
	}
	defer unlock()
	service, err := NewDesktopService(*profile)
	if err != nil {
		log.Fatal(err)
	}
	openOnStart := !*background && *health == "" && os.Getenv(update.BackgroundLaunchEnv) != "1"
	instance, err := claimDesktopInstance(*profile, openOnStart)
	if err != nil {
		log.Fatal(err)
	}
	if instance == 0 {
		if *health != "" {
			log.Fatal("设备更新试运行无法启动：已有设备实例")
		}
		return
	}
	defer windows.CloseHandle(instance)

	var quitting atomic.Bool
	options := application.Options{
		Name:        "Agent Muster",
		Description: "设备 Agent 管理",
		Services:    []application.Service{application.NewService(service)},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(distAssets()),
		},
		ShouldQuit: func() bool {
			return quitting.Load()
		},
	}
	app := application.New(options)

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Agent Muster",
		Frameless:        true,
		Hidden:           true,
		Width:            760,
		Height:           680,
		MinWidth:         minDeviceWindowWidth,
		MinHeight:        minDeviceWindowHeight,
		BackgroundColour: application.NewRGB(247, 248, 250),
		URL:              "/",
	})
	placement := newWindowPlacement(*profile, app, window)
	quit := func() {
		if quitting.CompareAndSwap(false, true) {
			app.Quit()
		}
	}
	service.updateHealth = *health
	service.quitForUpdate = quit
	service.setWindow(window)
	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if !quitting.Load() {
			event.Cancel()
			placement.Save()
			window.Hide()
		}
	})
	app.Event.OnApplicationEvent(events.Windows.ApplicationStarted, func(_ *application.ApplicationEvent) {
		if openOnStart {
			placement.Open()
		}
	})
	stopWatching, err := watchDesktopOpen(instance, func() {
		if !quitting.Load() {
			placement.Open()
		}
	})
	if err != nil {
		log.Fatal(err)
	}
	defer stopWatching()

	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetLabel("Agent Muster")
	tray.OnClick(placement.Open)
	menu := app.NewMenu()
	menu.Add("打开设备状态").OnClick(func(_ *application.Context) {
		placement.Open()
	})
	addStartupMenu(app, menu, *profile)
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(_ *application.Context) {
		quit()
	})
	tray.SetMenu(menu)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

func distAssets() fs.FS {
	assets, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		log.Fatal(err)
	}
	return assets
}
