//go:build windows

package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"os"
	"sync/atomic"

	"github.com/baiyucraft/agent-muster/internal/daemon/update"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/windows/icon.ico
var trayIcon []byte

func main() {
	// 更新辅助进程由同一程序启动，处理替换请求时无需初始化桌面窗口。
	if handled, err := update.HandleHelper(os.Args[1:]); handled {
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	profile := flag.String("profile", "default", "本机设备配置")
	health := flag.String("update-health", "", "")
	flag.Parse()
	unlock, err := update.LockInstallation(*health)
	if err != nil {
		log.Fatal(err)
	}
	defer unlock()
	service, err := NewDesktopService(*profile)
	if err != nil {
		log.Fatal(err)
	}

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
	quit := func() {
		quitting.Store(true)
		app.Quit()
	}
	service.updateHealth = *health
	service.quitForUpdate = quit

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Agent Muster",
		Frameless:        true,
		Width:            760,
		Height:           680,
		MinWidth:         620,
		MinHeight:        580,
		BackgroundColour: application.NewRGB(247, 248, 250),
		URL:              "/",
	})
	service.setWindow(window)
	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if !quitting.Load() {
			event.Cancel()
			window.Hide()
		}
	})
	window.Show()

	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetLabel("Agent Muster")
	menu := app.NewMenu()
	menu.Add("打开设备状态").OnClick(func(_ *application.Context) {
		window.Show()
	})
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(_ *application.Context) {
		quit()
	})
	tray.SetMenu(menu)
	tray.AttachWindow(window)

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
