//go:build windows

package main

import (
	"context"
	"errors"
	"runtime"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/update"
	"github.com/baiyucraft/agent-muster/internal/daemon/work"
	"github.com/baiyucraft/agent-muster/internal/deviceupdate"
)

// UpdateStatus 是桌面端最后一次记录的更新进度或安装结果。
type UpdateStatus struct {
	Version string `json:"version"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

func (service *DesktopService) pollUpdates(ctx context.Context, client *control.Client, identity control.Identity, worker *work.Worker, generation uint64) {
	if !deviceupdate.ValidVersion(update.Version) || service.quitForUpdate == nil {
		return
	}
	var pending *deviceupdate.Report
	lastResult := ""
	ticker := time.NewTicker(control.HeartbeatInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if result, err := update.LastResult(service.profile); err == nil && result.AssignmentID != "" && result.AssignmentID != lastResult && pending == nil {
			pending = &deviceupdate.Report{AssignmentID: result.AssignmentID, Status: result.State, Error: result.Error}
			lastResult = result.AssignmentID
			service.setUpdateProgress(generation, result.Version, *pending)
		}
		target, err := client.CheckUpdate(ctx, identity, deviceupdate.CheckRequest{Version: update.Version, Report: pending})
		if errors.Is(err, control.ErrUnauthorized) {
			return
		}
		if err == nil {
			// Hub 成功响应后才清除待上报结果，请求失败时在下次轮询重试。
			pending = nil
			if target != nil && target.Version != update.Version {
				var restarting bool
				pending, restarting = service.installUpdate(ctx, client, identity, worker, generation, *target)
				if restarting {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (service *DesktopService) installUpdate(ctx context.Context, client *control.Client, identity control.Identity, worker *work.Worker, generation uint64, target deviceupdate.Target) (*deviceupdate.Report, bool) {
	progress := func(state, message string) deviceupdate.Report {
		report := deviceupdate.Report{AssignmentID: target.ID, Status: state, Error: message}
		service.setUpdateProgress(generation, target.Version, report)
		return report
	}
	// 每次上报阶段后重新核对指派 ID，管理员替换指派时停止推进旧更新。
	report := func(state string) (*deviceupdate.Target, error) {
		value := progress(state, "")
		return client.CheckUpdate(ctx, identity, deviceupdate.CheckRequest{Version: update.Version, Report: &value})
	}
	fail := func(message string) (*deviceupdate.Report, bool) {
		value := progress(deviceupdate.StatusFailed, message)
		return &value, false
	}

	key, err := update.LoadPublicKey()
	if err != nil {
		return fail("设备缺少可信的更新发布公钥")
	}
	if worker == nil {
		return fail("设备工作进程未就绪")
	}
	if current, err := report(deviceupdate.StatusDownloading); err != nil || current == nil || current.ID != target.ID {
		return nil, false
	}
	download, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	offer, err := update.FetchOffer(download, target.ManifestURL)
	if err != nil {
		return fail("无法下载更新清单")
	}
	if offer.Manifest.Version != target.Version || update.Verify(offer, key, update.Version, runtime.GOOS+"/"+runtime.GOARCH) != nil {
		return fail("更新发布签名或版本校验失败")
	}
	prepared, err := update.Stage(download, offer, key)
	if err != nil {
		return fail("无法下载或验证更新文件")
	}
	started := false
	defer func() {
		if !started {
			prepared.Discard()
		}
	}()
	if current, err := report(deviceupdate.StatusWaiting); err != nil || current == nil || current.ID != target.ID {
		return nil, false
	}
	release, err := worker.Drain(ctx)
	if err != nil {
		return nil, false
	}
	defer func() {
		if !started {
			release()
		}
	}()
	if current, err := report(deviceupdate.StatusApplying); err != nil || current == nil || current.ID != target.ID {
		return nil, false
	}
	if err := prepared.Start(service.profile, target.ID); err != nil {
		return fail("无法启动本机更新程序")
	}
	started = true
	// Wails 退出时会等待轮询协程结束，因此由独立协程发起退出。
	go service.quitForUpdate()
	return nil, true
}

func (service *DesktopService) setUpdateProgress(generation uint64, version string, report deviceupdate.Report) {
	service.updateForGeneration(generation, func(status *DeviceStatus) {
		status.Update = &UpdateStatus{Version: version, State: report.Status, Error: report.Error}
	})
}
