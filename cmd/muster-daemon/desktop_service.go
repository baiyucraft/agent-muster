//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon"
	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/credentials"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"github.com/baiyucraft/agent-muster/internal/daemon/update"
	"github.com/baiyucraft/agent-muster/internal/daemon/work"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// DeviceStatus 是提供给桌面界面的设备配对、Agent、运行和更新状态快照。
type DeviceStatus struct {
	Paired        bool             `json:"paired"`
	HubURL        string           `json:"hubUrl"`
	DeviceID      string           `json:"deviceId"`
	Connection    string           `json:"connection"`
	LastHeartbeat *time.Time       `json:"lastHeartbeat"`
	Agents        []runner.Agent   `json:"agents"`
	ProbedAt      *time.Time       `json:"probedAt"`
	LastError     string           `json:"lastError"`
	Directories   []work.Directory `json:"directories"`
	LatestRun     *work.RunStatus  `json:"latestRun"`
	WorkError     string           `json:"workError"`
	PeerAddress   string           `json:"peerAddress"`
	PeerTransport string           `json:"peerTransport"`
	Version       string           `json:"version"`
	Update        *UpdateStatus    `json:"update"`
}

// DesktopService 管理桌面绑定和已配对设备的后台循环。
type DesktopService struct {
	store         credentials.Store
	profile       string
	peerAddress   string
	updateHealth  string
	quitForUpdate func()

	mu          sync.RWMutex
	status      DeviceStatus
	rootContext context.Context
	window      application.Window
	stop        context.CancelFunc
	generation  uint64

	pairMu    sync.Mutex
	refreshMu sync.Mutex
	worker    *work.Worker
	runDone   chan struct{}
}

// NewDesktopService 加载指定配置的凭据存储和设备互联偏好，由 ServiceStartup 启动后台循环。
func NewDesktopService(profile string) (*DesktopService, error) {
	store, err := credentials.New(profile)
	if err != nil {
		return nil, err
	}
	peerAddress, err := loadPeerAddress(profile)
	if err != nil {
		return nil, err
	}
	return &DesktopService{
		store:       store,
		profile:     profile,
		peerAddress: peerAddress,
		status:      DeviceStatus{Connection: "unpaired", Agents: []runner.Agent{}, Directories: []work.Directory{}, PeerAddress: peerAddress, Version: update.Version},
	}, nil
}

func loadPeerAddress(profile string) (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, "Agent Muster", profile, "peer-address.txt")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

// SetPeerAddress 保存直连地址。传入空字符串会清除地址，下次启动连接时使用自动互联。
func (service *DesktopService) SetPeerAddress(address string) error {
	address, err := work.NormalizePeerAddress(address)
	if err != nil {
		return err
	}
	service.pairMu.Lock()
	defer service.pairMu.Unlock()
	root, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	path := filepath.Join(root, "Agent Muster", service.profile, "peer-address.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if address == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		file, err := os.CreateTemp(filepath.Dir(path), "peer-address-*.txt")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		_, writeErr := file.WriteString(address + "\n")
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return err
		}
		if err := os.Rename(file.Name(), path); err != nil {
			return err
		}
	}
	service.mu.Lock()
	service.peerAddress = address
	service.status.PeerAddress = address
	service.mu.Unlock()
	return nil
}

// ServiceStartup 用已保存的凭据恢复设备连接，启动错误通过 GetStatus 提供给桌面界面。
func (service *DesktopService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	service.mu.Lock()
	service.rootContext = ctx
	service.mu.Unlock()

	data, err := service.store.Load()
	if errors.Is(err, credentials.ErrNotEnrolled) {
		return nil
	}
	if err != nil {
		service.setError(err)
		return nil
	}
	var identity control.Identity
	if err := json.Unmarshal(data, &identity); err != nil || !identity.Valid() {
		service.setError(errors.New("已保存的设备凭据格式无效，请重新配对"))
		return nil
	}
	if err := service.startReporter(identity); err != nil {
		service.setError(err)
	}
	return nil
}

// ServiceShutdown 取消设备后台循环并等待它们退出，然后关闭本机运行记录。
func (service *DesktopService) ServiceShutdown() error {
	service.pairMu.Lock()
	defer service.pairMu.Unlock()
	service.mu.Lock()
	stop := service.stop
	done := service.runDone
	service.stop = nil
	service.runDone = nil
	worker := service.worker
	service.worker = nil
	service.mu.Unlock()
	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}
	if worker != nil {
		return worker.Close()
	}
	return nil
}

// GetStatus 从本机存储刷新运行摘要并返回设备状态快照。
func (service *DesktopService) GetStatus() DeviceStatus {
	service.refreshLocalState()
	service.mu.RLock()
	defer service.mu.RUnlock()
	status := service.status
	if status.Update != nil {
		progress := *status.Update
		status.Update = &progress
	}
	status.Agents = append([]runner.Agent(nil), status.Agents...)
	status.Directories = append([]work.Directory(nil), status.Directories...)
	if status.LatestRun != nil {
		latest := *status.LatestRun
		status.LatestRun = &latest
	}
	return status
}

// Pair 兑换一次性配对码，把设备凭据提交到本机存储后启动后台循环。
func (service *DesktopService) Pair(hubURL, code string) error {
	service.pairMu.Lock()
	defer service.pairMu.Unlock()

	service.mu.RLock()
	paired := service.status.Paired
	connection := service.status.Connection
	service.mu.RUnlock()
	if paired && connection != "revoked" {
		return errors.New("此设备已配对")
	}

	client, err := control.NewClient(hubURL)
	if err != nil {
		return err
	}
	var identity control.Identity
	err = service.store.DuringEnrollment(func() error {
		ctx, cancel := context.WithTimeout(service.applicationContext(), 15*time.Second)
		defer cancel()
		identity, err = client.Enroll(ctx, code, localHostname())
		if err != nil {
			return err
		}
		data, err := json.Marshal(identity)
		if err != nil {
			return err
		}
		return service.store.Save(data)
	})
	if err != nil {
		return err
	}
	return service.startReporter(identity)
}

// RefreshAgents 重新探测本机 CLI，收到 Hub 确认后才更新清单和探测时间。
func (service *DesktopService) RefreshAgents() error {
	service.refreshMu.Lock()
	defer service.refreshMu.Unlock()

	service.mu.RLock()
	paired := service.status.Paired
	service.mu.RUnlock()
	if !paired {
		return errors.New("设备尚未配对")
	}
	identityData, err := service.store.Load()
	if err != nil {
		service.setError(err)
		return err
	}
	var identity control.Identity
	if err := json.Unmarshal(identityData, &identity); err != nil || !identity.Valid() {
		return errors.New("已保存的设备凭据格式无效")
	}
	client, err := control.NewClient(identity.HubURL)
	if err != nil {
		service.setError(err)
		return err
	}
	service.updateStatus(func(status *DeviceStatus) {
		status.LastError = ""
	})
	ctx, cancel := context.WithTimeout(service.applicationContext(), 45*time.Second)
	defer cancel()
	agents := runner.Discover(ctx)
	if err := client.ReportInventory(ctx, identity, agents); err != nil {
		service.setError(err)
		return err
	}
	now := time.Now().UTC()
	service.updateStatus(func(status *DeviceStatus) {
		status.Agents = append([]runner.Agent(nil), agents...)
		status.ProbedAt = &now
		status.LastError = ""
	})
	return nil
}

// AddWorkDirectory 打开目录选择器并登记选中的目录。取消选择时返回零值和 nil。
func (service *DesktopService) AddWorkDirectory() (work.Directory, error) {
	service.mu.RLock()
	worker := service.worker
	service.mu.RUnlock()
	if worker == nil {
		return work.Directory{}, errors.New("本机执行记录尚未初始化")
	}
	path, err := application.Get().Dialog.OpenFile().
		SetTitle("登记 Agent 工作目录").
		CanChooseDirectories(true).
		CanChooseFiles(false).
		AttachToWindow(service.window).
		PromptForSingleSelection()
	if err != nil {
		return work.Directory{}, err
	}
	if path == "" {
		return work.Directory{}, nil
	}
	service.mu.RLock()
	current := worker == service.worker
	service.mu.RUnlock()
	if !current {
		return work.Directory{}, errors.New("设备连接已更换，请重新登记目录")
	}
	ctx, cancel := context.WithTimeout(service.applicationContext(), 5*time.Second)
	defer cancel()
	directory, err := worker.RegisterDirectory(ctx, path)
	if err != nil {
		return work.Directory{}, err
	}
	service.refreshLocalState()
	return directory, nil
}

func (service *DesktopService) setWindow(window application.Window) {
	service.mu.Lock()
	service.window = window
	service.mu.Unlock()
}

func (service *DesktopService) refreshLocalState() {
	service.mu.RLock()
	worker := service.worker
	generation := service.generation
	service.mu.RUnlock()
	if worker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(service.applicationContext(), 5*time.Second)
	defer cancel()
	directories, err := worker.Directories(ctx)
	if err != nil {
		service.updateForGeneration(generation, func(status *DeviceStatus) { status.WorkError = err.Error() })
		return
	}
	latest, err := worker.LatestRun(ctx)
	if err != nil {
		service.updateForGeneration(generation, func(status *DeviceStatus) { status.WorkError = err.Error() })
		return
	}
	service.updateForGeneration(generation, func(status *DeviceStatus) {
		status.Directories = directories
		status.LatestRun = latest
	})
}

func (service *DesktopService) startReporter(identity control.Identity) error {
	if !identity.Valid() {
		return errors.New("设备凭据无效")
	}
	client, err := control.NewClient(identity.HubURL)
	if err != nil {
		return err
	}
	service.mu.RLock()
	peerAddress := service.peerAddress
	service.mu.RUnlock()
	service.mu.Lock()
	oldStop, oldDone, oldWorker := service.stop, service.runDone, service.worker
	service.stop = nil
	service.runDone = nil
	service.worker = nil
	service.mu.Unlock()
	if oldStop != nil {
		oldStop()
	}
	if oldDone != nil {
		<-oldDone
	}
	if oldWorker != nil {
		_ = oldWorker.Close()
	}
	worker, workErr := service.openWorker(identity)
	if service.updateHealth != "" {
		// 新版本先打开本机运行记录，再确认启动健康，随后才开始领取任务。
		if workErr != nil {
			return workErr
		}
		if err := update.MarkHealthy(service.updateHealth); err != nil {
			return errors.Join(err, worker.Close())
		}
		service.updateHealth = ""
	}
	service.mu.Lock()
	ctx, stop := context.WithCancel(service.rootContext)
	service.stop = stop
	done := make(chan struct{})
	service.runDone = done
	service.worker = worker
	service.generation++
	generation := service.generation
	service.status.Paired = true
	service.status.HubURL = identity.HubURL
	service.status.DeviceID = identity.DeviceID
	service.status.Connection = "checking"
	service.status.LastHeartbeat = nil
	service.status.ProbedAt = nil
	service.status.Agents = []runner.Agent{}
	service.status.LastError = ""
	service.status.Directories = []work.Directory{}
	service.status.LatestRun = nil
	service.status.WorkError = ""
	service.status.PeerTransport = "connecting"
	service.status.Update = nil
	if workErr != nil {
		service.status.WorkError = workErr.Error()
	}
	service.mu.Unlock()

	var loops sync.WaitGroup
	loops.Add(1)
	go func() {
		defer loops.Done()
		if err := daemon.RunWithObserver(ctx, client, identity, func(event daemon.Event) {
			service.applyEvent(generation, event)
		}); err != nil && ctx.Err() == nil {
			service.updateForGeneration(generation, func(status *DeviceStatus) {
				status.Connection = "revoked"
				status.LastError = err.Error()
			})
		}
	}()
	updateCtx, stopUpdates := context.WithCancel(ctx)
	if worker != nil {
		loops.Add(1)
		go func() {
			defer loops.Done()
			defer stopUpdates()
			updateWorkError := func(runErr error) {
				service.updateForGeneration(generation, func(status *DeviceStatus) {
					switch {
					case runErr == nil:
						status.WorkError = ""
					case work.APIUnavailable(runErr):
						status.WorkError = "当前 Hub 尚未启用任务派发"
					default:
						status.WorkError = runErr.Error()
					}
				})
			}
			peer := work.PeerConfig{
				Address: peerAddress,
				OnStatus: func(transport string) {
					service.updateForGeneration(generation, func(status *DeviceStatus) {
						status.PeerTransport = transport
					})
				},
			}
			if err := worker.RunWithPeer(ctx, client, identity, peer, updateWorkError); err != nil && ctx.Err() == nil {
				updateWorkError(err)
			}
		}()
	}
	loops.Add(1)
	go func() {
		defer loops.Done()
		defer stopUpdates()
		service.pollUpdates(updateCtx, client, identity, worker, generation)
	}()
	go func() {
		loops.Wait()
		close(done)
	}()
	return nil
}

func (service *DesktopService) openWorker(identity control.Identity) (*work.Worker, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	scope := sha256.Sum256([]byte(identity.HubURL + "\x00" + identity.DeviceID))
	return work.Open(filepath.Join(root, "Agent Muster", service.profile, hex.EncodeToString(scope[:16])))
}

func (service *DesktopService) applyEvent(generation uint64, event daemon.Event) {
	service.updateForGeneration(generation, func(status *DeviceStatus) {
		switch event.Kind {
		case daemon.EventHeartbeat:
			t := event.At.UTC()
			status.LastHeartbeat = &t
			status.Connection = "online"
			status.LastError = ""
		case daemon.EventInventory:
			t := event.At.UTC()
			status.Agents = append([]runner.Agent(nil), event.Agents...)
			status.ProbedAt = &t
			status.LastError = ""
		case daemon.EventError:
			status.LastError = event.Err.Error()
			if event.Source == "heartbeat" {
				status.Connection = "offline"
			}
		case daemon.EventRevoked:
			status.Connection = "revoked"
			status.LastError = event.Err.Error()
		}
	})
}

func (service *DesktopService) updateForGeneration(generation uint64, update func(*DeviceStatus)) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if generation == service.generation {
		update(&service.status)
	}
}

func (service *DesktopService) updateStatus(update func(*DeviceStatus)) {
	service.mu.Lock()
	defer service.mu.Unlock()
	update(&service.status)
}

func (service *DesktopService) setError(err error) {
	service.updateStatus(func(status *DeviceStatus) {
		status.Connection = "offline"
		status.LastError = fmt.Sprint(err)
	})
}

func (service *DesktopService) applicationContext() context.Context {
	service.mu.RLock()
	ctx := service.rootContext
	service.mu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func localHostname() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "windows-device"
	}
	return hostname
}
