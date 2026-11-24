package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

const processProofFlag = "--muster-process-proof-helper"

var processProofName = regexp.MustCompile(`^exit-[0-9a-f]{32,64}\.proof$`)

// ProcessExitConfirmed 只接受与该预留文件名绑定的完整退出证据。文件尚未写入时返回 false。
func ProcessExitConfirmed(path string) (bool, error) {
	if err := validateProcessProofPath(path); err != nil {
		return false, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return false, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return false, errors.New("退出证据必须是普通文件")
	}
	expected := processProofText(path)
	data, err := io.ReadAll(io.LimitReader(file, int64(len(expected)+1)))
	if err != nil {
		return false, err
	}
	if string(data) != expected {
		return false, errors.New("退出证据内容无效")
	}
	return true, nil
}

func validateProcessProofPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !processProofName.MatchString(filepath.Base(path)) {
		return errors.New("退出证据路径无效")
	}
	directory, err := windows.UTF16PtrFromString(filepath.Dir(path))
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(directory)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("退出证据目录必须是普通目录")
	}
	return nil
}

func processProofText(path string) string {
	return "muster-process-exit-v1\n" + filepath.Base(path) + "\n"
}

// writeProcessProof 先同步完整内容，再发布到预留路径。父进程与辅助进程重复提交时复用相同证据。
func writeProcessProof(path string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".process-exit-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(processProofText(path))
	syncErr := file.Sync()
	if err := errors.Join(writeErr, syncErr, file.Close()); err != nil {
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
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		if confirmed, readErr := ProcessExitConfirmed(path); !confirmed {
			return errors.Join(err, readErr)
		}
	}
	return nil
}

type processGuardian struct {
	ready   windows.Handle
	done    windows.Handle
	process windows.Handle
}

// startProcessGuardian 确认独立进程已持有本轮 Job 和设备进程句柄。调用成功后才能启动 CLI。
func startProcessGuardian(ctx context.Context, job windows.Handle, path string) (*processGuardian, error) {
	owner, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, true, windows.GetCurrentProcessId())
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(owner)
	current := windows.CurrentProcess()
	var inheritedJob windows.Handle
	if err := windows.DuplicateHandle(current, job, current, &inheritedJob, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	defer windows.CloseHandle(inheritedJob)

	inherit := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	ready, err := windows.CreateEvent(&inherit, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	done, err := windows.CreateEvent(&inherit, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(ready)
		return nil, err
	}
	process, err := launchProcessGuardian(path, []windows.Handle{owner, inheritedJob, done, ready})
	if err != nil {
		windows.CloseHandle(ready)
		windows.CloseHandle(done)
		return nil, err
	}
	guardian := &processGuardian{ready: ready, done: done, process: process}
	if err := awaitProcessGuardian(ctx, guardian.process, ready); err != nil {
		// CLI 尚未启动。让辅助进程结束，即使就绪回执与取消并发到达。
		_ = guardian.finish(false)
		return nil, err
	}
	return guardian, nil
}

func launchProcessGuardian(path string, handles []windows.Handle) (windows.Handle, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, err
	}
	exe, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return 0, err
	}
	args := []string{executable, processProofFlag, path}
	for _, handle := range handles {
		args = append(args, strconv.FormatUint(uint64(handle), 10))
	}
	command, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return 0, err
	}
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, err
	}
	defer attributes.Delete()
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return 0, err
	}
	startup := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{}))},
		ProcThreadAttributeList: attributes.List()}
	var process windows.ProcessInformation
	var env [2]uint16
	flags := uint32(createNoWindow | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := windows.CreateProcess(exe, command, nil, nil, true, flags, &env[0], nil, &startup.StartupInfo, &process); err != nil {
		return 0, err
	}
	windows.CloseHandle(process.Thread)
	return process.Process, nil
}

func awaitProcessGuardian(ctx context.Context, process, ready windows.Handle) error {
	// 辅助进程一般在毫秒内确认就绪。本机无法启动时有限等待，并遵循本轮取消。
	for attempt := 0; attempt < 50; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := windows.WaitForMultipleObjects([]windows.Handle{process, ready}, false, 100)
		if err != nil {
			return err
		}
		switch state {
		case windows.WAIT_OBJECT_0:
			return errors.New("退出核对进程未确认就绪即退出")
		case windows.WAIT_OBJECT_0 + 1:
			return ctx.Err()
		case uint32(windows.WAIT_TIMEOUT):
			continue
		default:
			return fmt.Errorf("无法确认退出核对进程状态 (%d)", state)
		}
	}
	return errors.New("等待退出核对进程超时")
}

// finish 通知辅助进程关闭。退出证据落盘后须等待辅助进程释放设备可执行文件。
// 状态未核对时保留辅助进程继续核对，设备关闭时由其续写证据。
func (guardian *processGuardian) finish(proofWritten bool) error {
	defer windows.CloseHandle(guardian.ready)
	defer windows.CloseHandle(guardian.done)
	defer windows.CloseHandle(guardian.process)
	if !proofWritten {
		return windows.SetEvent(guardian.done)
	}
	// 清除就绪信号表示设备已提交证据，辅助进程无需重复写入。
	resetErr := windows.ResetEvent(guardian.ready)
	doneErr := windows.SetEvent(guardian.done)
	state, waitErr := windows.WaitForSingleObject(guardian.process, 1000)
	if waitErr == nil && state == windows.WAIT_OBJECT_0 {
		return nil
	}
	// 设备已确认 Job 退出且持久化证据，此时可终止失去响应的辅助进程。
	stopErr := windows.TerminateProcess(guardian.process, 1)
	state, waitErr = windows.WaitForSingleObject(guardian.process, 1000)
	if waitErr == nil && state == windows.WAIT_OBJECT_0 {
		return nil
	}
	return errors.Join(fmt.Errorf("退出核对进程未结束 (%d)", state), resetErr, doneErr, stopErr, waitErr)
}

// HandleProcessProofHelper 在设备入口接管内部辅助进程参数，并持有实际传入的内核对象。
// args 从可执行文件后的第一个参数开始。handled 为 true 时调用方应结束进程。
func HandleProcessProofHelper(args []string) (bool, error) {
	if len(args) == 0 || args[0] != processProofFlag {
		return false, nil
	}
	if len(args) != 6 {
		return true, errors.New("退出核对进程参数无效")
	}
	path := args[1]
	if err := validateProcessProofPath(path); err != nil {
		return true, err
	}
	var handles [4]windows.Handle
	for i, arg := range args[2:] {
		value, err := strconv.ParseUint(arg, 10, strconv.IntSize)
		if err != nil || value == 0 || windows.Handle(value) == windows.InvalidHandle {
			return true, errors.New("退出核对进程句柄无效")
		}
		handles[i] = windows.Handle(value)
		defer windows.CloseHandle(handles[i])
	}
	owner, job, done, ready := handles[0], handles[1], handles[2], handles[3]
	if _, err := windows.GetProcessId(owner); err != nil {
		return true, err
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)), nil); err != nil {
		return true, err
	}
	if err := windows.SetEvent(ready); err != nil {
		return true, err
	}
	state, err := windows.WaitForMultipleObjects([]windows.Handle{done, owner}, false, windows.INFINITE)
	if err != nil || (state != windows.WAIT_OBJECT_0 && state != windows.WAIT_OBJECT_0+1) {
		return true, errors.Join(errors.New("无法等待设备退出"), err)
	}
	// 就绪信号已被清除时，设备已提交证据，可在设备回收文件后退出。
	confirmed, err := windows.WaitForSingleObject(ready, 0)
	if confirmed == uint32(windows.WAIT_TIMEOUT) && err == nil {
		return true, nil
	}
	if confirmed, _ := ProcessExitConfirmed(path); confirmed {
		return true, nil
	}
	if err := stopProcessTree(job); err != nil {
		return true, err
	}
	return true, writeProcessProof(path)
}
