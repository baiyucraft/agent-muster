//go:build windows

package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/baiyucraft/agent-muster/internal/deviceupdate"
)

const (
	// 更新器独占第一个字节，阻止常规启动进入安装目录。
	updateLockByte uint32 = iota
	// 桌面进程共享第二个字节，更新器仅在替换文件时独占它。
	runningLockByte
)

const (
	stateLockName = "install.lock"
	finishedName  = "finished"
	readyTimeout  = 10 * time.Second
	healthTimeout = 45 * time.Second
)

// Stage 验证清单后下载归档并校验两个程序，将它们暂存到安装卷。
// 下载期间可通过 ctx 取消。返回错误时会清理本次暂存。
func Stage(ctx context.Context, offer Offer, key ed25519.PublicKey) (*Prepared, error) {
	if err := Verify(offer, key, Version, platform()); err != nil {
		return nil, err
	}
	install, err := installDirectory()
	if err != nil {
		return nil, err
	}
	if err := checkInstall(install); err != nil {
		return nil, err
	}
	dir, err := stateDirectory(install)
	if err != nil {
		return nil, err
	}
	pruneFinishedStages(dir)
	stage, err := os.MkdirTemp(dir, "release-")
	if err != nil {
		return nil, err
	}
	prepared := &Prepared{install: install, stage: stage, offer: offer}
	archivePath := filepath.Join(stage, "release.zip")
	if err := downloadArchive(ctx, offer.Manifest, archivePath); err != nil {
		prepared.Discard()
		return nil, err
	}
	if err := extract(archivePath, stage, offer.Manifest); err != nil {
		prepared.Discard()
		return nil, err
	}
	if err := os.Remove(archivePath); err != nil {
		prepared.Discard()
		return nil, err
	}
	return prepared, nil
}

// Start 在当前任务已安全停止后启动独立更新器，最多等待 10 秒确认接管。
// 成功后暂存目录交给更新器，调用方应立即退出，以便更新器独占安装目录。
// 失败时暂存目录仍由调用方负责清理。
func (prepared *Prepared) Start(profile, assignmentID string) error {
	if !profilePattern.MatchString(profile) || !assignmentPattern.MatchString(assignmentID) || prepared.stage == "" {
		return errors.New("invalid release application request")
	}
	helperPath := filepath.Join(prepared.stage, helperName)
	if err := replaceWithFile(helperPath, filepath.Join(prepared.install, AppName)); err != nil {
		return err
	}
	recordPath := filepath.Join(prepared.stage, recordName)
	if err := atomicWriteRecord(recordPath, applicationRecord{
		Offer: prepared.offer, Profile: profile, AssignmentID: assignmentID,
	}); err != nil {
		return err
	}
	if err := startHelper(prepared.stage, false); err != nil {
		return err
	}
	prepared.stage = ""
	return nil
}

func startHelper(stage string, recovery bool) error {
	_ = os.Remove(filepath.Join(stage, readyName))
	cmd := quietCommand(filepath.Join(stage, helperName), helperFlag, strconv.Itoa(os.Getpid()), strconv.FormatBool(recovery))
	cmd.Dir = stage
	logFile, err := os.OpenFile(filepath.Join(stage, "helper.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Process.Release()
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(stage, readyName)); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return errors.New("update helper did not start")
}

func quietCommand(path string, args ...string) *exec.Cmd {
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd
}

// LockInstallation 防止运行中的桌面进程与文件替换冲突。
// 调用方须持有返回的释放函数直到进程退出。有未完成安装日志时启动恢复进程并返回错误。
func LockInstallation(healthPath string) (func(), error) {
	install, err := installDirectory()
	if err != nil && healthPath == "" {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	state, err := stateDirectory(install)
	if err != nil {
		if healthPath == "" && errors.Is(err, os.ErrPermission) {
			// 只读安装仍允许启动桌面程序。
			return func() {}, nil
		}
		return nil, err
	}
	if healthPath != "" {
		// 试运行由更新器启动，仅持有运行锁。更新器保留更新锁以拦截常规启动。
		if _, err := validateHealthPath(state, healthPath); err != nil {
			return nil, err
		}
		return lock(state, runningLockByte, false)
	}
	gate, err := lock(state, updateLockByte, false)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return func() {}, nil
		}
		return nil, fmt.Errorf("device update is in progress: %w", err)
	}
	stage, err := activeStage(state)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		gate()
		return nil, err
	}
	if stage != "" {
		gate()
		if err := startHelper(stage, true); err != nil {
			return nil, fmt.Errorf("recover interrupted device update: %w", err)
		}
		return nil, errors.New("recovering interrupted device update")
	}
	release, err := lock(state, runningLockByte, false)
	gate()
	return release, err
}

// MarkHealthy 在试运行的本地工作器就绪后写入健康标记，并等待更新器落盘成功结果。
// 空路径表示常规启动。试运行必须等此函数成功后再开始领取任务。
func MarkHealthy(path string) error {
	if path == "" {
		return nil
	}
	install, err := installDirectory()
	if err != nil {
		return err
	}
	stage, err := validateHealthPath(filepath.Join(install, updateDir), path)
	if err != nil {
		return err
	}
	record, err := validatedRecord(stage)
	if err != nil {
		return err
	}
	if record.Offer.Manifest.Version != Version {
		return errors.New("running device version does not match the signed trial version")
	}
	if err := atomicWriteRecord(path, Version); err != nil {
		return err
	}
	state := filepath.Dir(stage)
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		active, err := activeStage(state)
		// Windows 删除日志的瞬间可能仍报告共享冲突，等待下一次读取。
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			var result Result
			err := readRecord(resultPath(install, record.Profile), &result)
			if err == nil && result.AssignmentID == record.AssignmentID && result.Version == Version &&
				result.State == deviceupdate.StatusSucceeded {
				return nil
			}
			return errors.New("device update did not commit a successful startup")
		}
		if err != nil || active != stage {
			return errors.Join(errors.New("device update journal changed during startup"), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("device update did not commit before the startup deadline")
}

func validateHealthPath(state, path string) (string, error) {
	stage, err := activeStage(state)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(path, filepath.Join(stage, healthName)) {
		return "", errors.New("invalid device update health path")
	}
	return stage, nil
}

func activeStage(state string) (string, error) {
	var name string
	if err := readRecord(filepath.Join(state, activeName), &name); err != nil {
		return "", err
	}
	if !stagePattern.MatchString(name) {
		return "", errors.New("invalid active device update")
	}
	stage := filepath.Join(state, name)
	return stage, requireDirectory(stage)
}

// HandleHelper 在桌面参数解析和安装加锁前处理独立更新器参数。
// handled 为 true 时只执行更新器流程，返回的错误用于本机诊断。
func HandleHelper(args []string) (bool, error) {
	if len(args) == 0 || args[0] != helperFlag {
		return false, nil
	}
	if len(args) != 3 {
		return true, errors.New("invalid update helper invocation")
	}
	pid, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil || pid == 0 {
		return true, errors.New("invalid update parent process")
	}
	recovery, err := strconv.ParseBool(args[2])
	if err != nil {
		return true, err
	}
	return true, runHelper(uint32(pid), recovery)
}

func runHelper(pid uint32, recovery bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	stage := filepath.Dir(exe)
	state := filepath.Dir(stage)
	install := filepath.Dir(state)
	if filepath.Base(exe) != helperName || !stagePattern.MatchString(filepath.Base(stage)) || filepath.Base(state) != updateDir {
		return errors.New("invalid update helper location")
	}
	if err := checkInstall(install); err != nil {
		return err
	}
	if err := requireDirectory(stage); err != nil {
		return err
	}
	record, err := validatedRecord(stage)
	if err != nil {
		return err
	}
	key, err := loadPublicKey(install)
	if err != nil {
		return err
	}
	if err := Verify(record.Offer, key, Version, platform()); err != nil {
		return err
	}
	appPath := filepath.Join(install, AppName)
	parent, err := applicationProcess(pid, appPath)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	// 持有父进程句柄，再通知父进程退出，避免 PID 复用影响等待。
	gate, err := lock(state, updateLockByte, true)
	if err != nil {
		return err
	}
	defer gate()
	if err := checkActiveStage(state, stage, recovery); err != nil {
		return err
	}
	if err := atomicWriteRecord(filepath.Join(stage, readyName), true); err != nil {
		return err
	}
	if event, err := windows.WaitForSingleObject(parent, 30000); err != nil || event != windows.WAIT_OBJECT_0 {
		if !recovery {
			_ = writeResult(install, record, deviceupdate.StatusFailed)
			cleanStage(stage)
		}
		return errors.Join(errors.New("device did not shut down for update"), err)
	}
	status, err := applyOrRecover(install, state, stage, record, recovery)
	reportErr := writeResult(install, record, status)
	active, activeErr := activeStage(state)
	// 先落盘终态再删除安装日志。试运行以这两个条件判断更新是否提交。
	if status != deviceupdate.StatusFailed && reportErr == nil && activeErr == nil && active == stage {
		if err := os.Remove(filepath.Join(state, activeName)); err != nil {
			reportErr = err
		} else {
			active = ""
		}
	}
	gate()
	var restartErr error
	if (status != deviceupdate.StatusSucceeded || recovery) && active != stage {
		restartErr = restartApplication(install, record.Profile)
	}
	if active, _ := activeStage(state); active != stage {
		cleanStage(stage)
	}
	return errors.Join(err, reportErr, restartErr)
}

func writeResult(install string, record applicationRecord, status string) error {
	result := Result{AssignmentID: record.AssignmentID, Version: record.Offer.Manifest.Version, State: status}
	switch status {
	case deviceupdate.StatusFailed:
		result.Error = "更新程序未能完成安装，详情见设备本地日志"
	case deviceupdate.StatusRolledBack:
		result.Error = "新版启动检查未通过，已恢复上一个版本"
	}
	return atomicWriteRecord(resultPath(install, record.Profile), result)
}

func checkActiveStage(state, stage string, recovery bool) error {
	active, err := activeStage(state)
	if recovery && err == nil && active == stage {
		return nil
	}
	if !recovery && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.New("another update or recovery is already active")
}

func applyOrRecover(install, state, stage string, record applicationRecord, recovery bool) (string, error) {
	guard, err := lock(state, runningLockByte, true)
	if err != nil {
		return deviceupdate.StatusFailed, fmt.Errorf("another device window still uses the installation: %w", err)
	}
	defer guard()
	if recovery {
		// 健康标记表示上次试运行已完成检查，可补写成功结果。其余情况恢复备份。
		if healthy(stage, record.Offer.Manifest.Version) {
			return deviceupdate.StatusSucceeded, nil
		}
		if err := restore(install, stage); err != nil {
			return deviceupdate.StatusFailed, err
		}
		return deviceupdate.StatusRolledBack, errors.New("recovered interrupted update")
	}
	if err := verifyStagedFiles(stage, record.Offer.Manifest); err != nil {
		return deviceupdate.StatusFailed, err
	}
	helperHash, err := digestFile(filepath.Join(stage, helperName))
	if err != nil {
		return deviceupdate.StatusFailed, err
	}
	appHash, err := digestFile(filepath.Join(install, AppName))
	if err != nil || appHash != helperHash {
		return deviceupdate.StatusFailed, errors.Join(errors.New("installed device changed before update"), err)
	}
	if err := backup(install, stage); err != nil {
		return deviceupdate.StatusFailed, err
	}
	// 两个文件需要分别替换，写入日志后才能开始。中途退出时下一次启动据此恢复。
	if err := atomicWriteRecord(filepath.Join(state, activeName), filepath.Base(stage)); err != nil {
		return deviceupdate.StatusFailed, err
	}
	if err := installBinaries(stage, install); err != nil {
		return rollback(install, stage, err)
	}
	// 释放运行锁以允许试运行进程启动。更新锁仍由独立更新器持有。
	guard()
	if err := runTrial(install, stage, record); err != nil {
		return rollback(install, stage, err)
	}
	return deviceupdate.StatusSucceeded, nil
}

func verifyStagedFiles(stage string, manifest Manifest) error {
	for _, name := range executableNames {
		path := filepath.Join(stage, name)
		if err := requireRegularFile(path); err != nil {
			return err
		}
		digest, err := digestFile(path)
		if err != nil {
			return err
		}
		if digest != manifest.Files[name] {
			return fmt.Errorf("staged %s does not match its signed SHA-256", name)
		}
	}
	return nil
}

func backup(install, stage string) error {
	dir := filepath.Join(stage, backupDir)
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	return installBinaries(install, dir)
}

func restore(install, stage string) error {
	if err := installBinaries(filepath.Join(stage, backupDir), install); err != nil {
		return fmt.Errorf("could not restore the previous device release: %w", err)
	}
	return nil
}

func installBinaries(from, to string) error {
	for _, name := range executableNames {
		if err := replaceWithFile(filepath.Join(to, name), filepath.Join(from, name)); err != nil {
			return fmt.Errorf("replace %s: %w", name, err)
		}
	}
	return nil
}

func rollback(install, stage string, cause error) (string, error) {
	if err := restore(install, stage); err != nil {
		return deviceupdate.StatusFailed, errors.Join(cause, err)
	}
	return deviceupdate.StatusRolledBack, cause
}

func runTrial(install, stage string, record applicationRecord) error {
	cmd := quietCommand(filepath.Join(install, AppName), "-profile="+record.Profile, "-update-health="+filepath.Join(stage, healthName))
	cmd.Dir = install
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(healthTimeout)
	defer timeout.Stop()
	for {
		select {
		case err := <-exited:
			return errors.Join(errors.New("updated device exited before confirming startup"), err)
		case <-ticker.C:
			if healthy(stage, record.Offer.Manifest.Version) {
				return nil
			}
		case <-timeout.C:
			_ = cmd.Process.Kill()
			// 等新进程退出后才恢复旧文件。
			<-exited
			return errors.New("updated device did not confirm startup")
		}
	}
}

func healthy(stage, expectedVersion string) bool {
	var version string
	return readRecord(filepath.Join(stage, healthName), &version) == nil && version == expectedVersion
}

func restartApplication(install, profile string) error {
	cmd := quietCommand(filepath.Join(install, AppName), "-profile="+profile)
	cmd.Dir = install
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func cleanStage(stage string) {
	for _, name := range []string{readyName, healthName, recordName, CLIName, AppName, backupDir} {
		_ = os.RemoveAll(filepath.Join(stage, name))
	}
	_ = atomicWriteRecord(filepath.Join(stage, finishedName), true)
}

func pruneFinishedStages(state string) {
	entries, err := os.ReadDir(state)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !stagePattern.MatchString(entry.Name()) {
			continue
		}
		stage := filepath.Join(state, entry.Name())
		if requireDirectory(stage) != nil {
			continue
		}
		var finished bool
		if readRecord(filepath.Join(stage, finishedName), &finished) == nil && finished {
			_ = os.RemoveAll(stage)
		}
	}
}

func checkInstall(install string) error {
	for _, varName := range []string{"WINDIR", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		root := os.Getenv(varName)
		rel, err := filepath.Rel(root, install)
		if root != "" && err == nil && filepath.IsLocal(rel) {
			return errors.New("automatic updates require a writable per-user installation")
		}
	}
	if err := requireDirectory(install); err != nil {
		return err
	}
	for _, name := range executableNames {
		if err := requireRegularFile(filepath.Join(install, name)); err != nil {
			return err
		}
	}
	return nil
}

func stateDirectory(install string) (string, error) {
	state := filepath.Join(install, updateDir)
	if err := os.Mkdir(state, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return state, requireDirectory(state)
}

func requireDirectory(path string) error {
	value, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(value)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s must be a regular directory", filepath.Base(path))
	}
	return nil
}

func applicationProcess(pid uint32, path string) (windows.Handle, error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, err
	}
	name := make([]uint16, 32768)
	size := uint32(len(name))
	if err := windows.QueryFullProcessImageName(handle, 0, &name[0], &size); err != nil || !strings.EqualFold(windows.UTF16ToString(name[:size]), path) {
		windows.CloseHandle(handle)
		return 0, errors.Join(errors.New("update parent is not the installed device application"), err)
	}
	return handle, nil
}

func lock(dir string, offset uint32, exclusive bool) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, stateLockName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	rangeStart := windows.Overlapped{Offset: offset}
	var flags uint32 = windows.LOCKFILE_FAIL_IMMEDIATELY
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &rangeStart); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		if file != nil {
			_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &rangeStart)
			_ = file.Close()
			file = nil
		}
	}, nil
}

func replaceFile(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func openRecord(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// 允许同时读取记录和替换或删除记录，避免观察者阻塞更新器提交。
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
