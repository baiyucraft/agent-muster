package main

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"golang.org/x/sys/windows"
)

const (
	minDeviceWindowWidth  = 620
	minDeviceWindowHeight = 580
	windowSaveDelay       = 350 * time.Millisecond
	windowOpenFallback    = 2 * time.Second
)

// windowPlacement 在 Wails 主线程管理窗口状态。边界使用全局 DIP，且只记录普通窗口。
type windowPlacement struct {
	app    *application.App
	window *application.WebviewWindow
	path   string

	saved          application.Rect
	current        application.Rect
	saveTimer      *time.Timer
	saveGeneration uint64
	openTimer      *time.Timer
	openRequested  bool
	ready          bool
	opened         bool
	closed         bool
}

// newWindowPlacement 读取 profile 的本地窗口记录，并在退出前刷新最新普通窗口边界。
func newWindowPlacement(profile string, app *application.App, window *application.WebviewWindow) *windowPlacement {
	p := &windowPlacement{app: app, window: window}
	if root, err := os.UserConfigDir(); err == nil {
		p.path = filepath.Join(root, "Agent Muster", profile, "window-placement.json")
		p.saved = readWindowBounds(p.path)
	}
	window.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) {
		application.InvokeAsync(func() {
			p.ready = true
			if p.openRequested {
				p.open()
			}
		})
	})
	for _, event := range []events.WindowEventType{events.Common.WindowDidMove, events.Common.WindowDidResize} {
		window.OnWindowEvent(event, func(*application.WindowEvent) {
			application.InvokeAsync(p.boundsChanged)
		})
	}
	app.OnShutdown(func() {
		p.Save()
		p.closed = true
		if p.openTimer != nil {
			p.openTimer.Stop()
		}
	})
	return p
}

// Open 安排打开或聚焦窗口。首次显示时先恢复记录，等待 WebView 就绪以避免空白窗口。
func (p *windowPlacement) Open() {
	application.InvokeAsync(func() {
		if p.closed {
			return
		}
		p.openRequested = true
		if p.ready {
			p.open()
		} else if p.openTimer == nil {
			// 前端加载失败时仍允许用户打开原生窗口。
			p.openTimer = time.AfterFunc(windowOpenFallback, func() {
				application.InvokeAsync(func() {
					p.openTimer = nil
					if p.openRequested {
						p.open()
					}
				})
			})
		}
	})
}

func (p *windowPlacement) open() {
	if p.closed {
		return
	}
	bounds := p.window.Bounds()
	if !validWindowBounds(bounds) {
		// 原生窗口尚未创建，保留请求给 WindowRuntimeReady。
		return
	}
	p.openRequested = false
	if p.openTimer != nil {
		p.openTimer.Stop()
		p.openTimer = nil
	}

	if p.window.IsMinimised() {
		p.window.UnMinimise()
		bounds = p.window.Bounds()
	}
	if !p.window.IsMaximised() && !p.window.IsFullscreen() {
		screens := p.app.Screen.GetAll()
		target := bounds
		switch {
		case !p.opened && validWindowBounds(p.saved):
			target = fitWindowBounds(p.saved, screens)
		case !windowHeaderVisible(bounds, screens):
			target = fitWindowBounds(bounds, screens)
		}
		if target != bounds {
			p.window.SetBounds(target)
		}
	}
	p.opened = true
	p.window.Show()
	if !p.window.IsVisible() {
		// 旧更新器传入 SW_HIDE，Windows 会用它覆盖进程首次 ShowWindow 的可见状态。
		p.window.Show()
	}
	p.window.Focus()
}

// Save 提交最后一次有效边界。隐藏后或关闭前重复调用可安全跳过已保存记录。
func (p *windowPlacement) Save() {
	application.InvokeSync(func() {
		p.capture()
		p.saveGeneration++
		if p.saveTimer != nil {
			p.saveTimer.Stop()
			p.saveTimer = nil
		}
		p.flush()
	})
}

func (p *windowPlacement) capture() bool {
	if p.path == "" || p.closed || !p.opened || !p.window.IsVisible() ||
		p.window.IsMinimised() || p.window.IsMaximised() || p.window.IsFullscreen() {
		return false
	}
	bounds := p.window.Bounds()
	if !validWindowBounds(bounds) {
		return false
	}
	p.current = bounds
	return true
}

func (p *windowPlacement) boundsChanged() {
	if !p.capture() {
		return
	}
	p.saveGeneration++
	generation := p.saveGeneration
	if p.saveTimer != nil {
		p.saveTimer.Stop()
	}
	p.saveTimer = time.AfterFunc(windowSaveDelay, func() {
		application.InvokeAsync(func() {
			if p.closed || generation != p.saveGeneration {
				return
			}
			p.saveTimer = nil
			p.flush()
		})
	})
}

func (p *windowPlacement) flush() {
	if !validWindowBounds(p.current) || p.current == p.saved {
		return
	}
	if err := writeWindowBounds(p.path, p.current); err != nil {
		p.app.Logger.Warn("保存设备窗口位置失败", "error", err)
		return
	}
	p.saved = p.current
}

func readWindowBounds(path string) application.Rect {
	var bounds application.Rect
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &bounds) != nil || !validWindowBounds(bounds) {
		return application.Rect{}
	}
	return bounds
}

func writeWindowBounds(path string, bounds application.Rect) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "window-placement-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(bounds)
	if err == nil {
		err = file.Sync()
	}
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(file.Name())
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	// 同目录替换保留上次记录，直到新的完整文件可以替换它。
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func validWindowBounds(bounds application.Rect) bool {
	return bounds.Width > 0 && bounds.Height > 0 &&
		bounds.X >= math.MinInt32 && bounds.X <= math.MaxInt32-bounds.Width &&
		bounds.Y >= math.MinInt32 && bounds.Y <= math.MaxInt32-bounds.Height
}

// fitWindowBounds 选择重叠最大的工作区，完全不可见时回主屏居中。
func fitWindowBounds(bounds application.Rect, screens []*application.Screen) application.Rect {
	var fallback, target *application.Screen
	var maxOverlap int64
	for _, screen := range screens {
		area := screen.WorkArea
		if !validWindowBounds(area) {
			continue
		}
		if fallback == nil || screen.IsPrimary {
			fallback = screen
		}
		overlap := bounds.Intersect(area)
		overlapArea := int64(overlap.Width) * int64(overlap.Height)
		if overlapArea > maxOverlap {
			target = screen
			maxOverlap = overlapArea
		}
	}
	if fallback == nil {
		return bounds
	}
	if target == nil {
		target = fallback
	}
	area := target.WorkArea
	bounds.Width = min(max(bounds.Width, minDeviceWindowWidth), area.Width)
	bounds.Height = min(max(bounds.Height, minDeviceWindowHeight), area.Height)
	if maxOverlap == 0 {
		bounds.X = area.X + (area.Width-bounds.Width)/2
		bounds.Y = area.Y + (area.Height-bounds.Height)/2
	} else {
		bounds.X = min(max(bounds.X, area.X), area.X+area.Width-bounds.Width)
		bounds.Y = min(max(bounds.Y, area.Y), area.Y+area.Height-bounds.Height)
	}
	return bounds
}

// windowHeaderVisible 判断原位置是否仍保留足以拖动的窗口顶部。
func windowHeaderVisible(bounds application.Rect, screens []*application.Screen) bool {
	const minVisibleHeaderWidth = 64
	const minVisibleHeaderHeight = 20
	header := bounds
	header.Height = min(header.Height, minVisibleHeaderHeight)
	for _, screen := range screens {
		area := screen.WorkArea
		overlap := header.Intersect(area)
		if overlap.Width >= minVisibleHeaderWidth && overlap.Height >= minVisibleHeaderHeight {
			return true
		}
	}
	return false
}
