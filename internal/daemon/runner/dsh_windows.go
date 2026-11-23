package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PROC_THREAD_ATTRIBUTE_JOB_LIST assigns the process to this job atomically
// during creation, before any dsh code can run.
const processJobList = 0x0002000D

func runInJob(ctx context.Context, cmd *exec.Cmd, prompt string, started func() error, read func(io.Reader) error) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return 0, err
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer stdinR.Close()
	defer stdinW.Close()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer stdoutR.Close()
	defer stdoutW.Close()

	capture := diagnostics(ctx)
	var stdoutLog, stderrLog *streamLog
	var captureIncomplete bool
	var stderrR, stderrW *os.File
	if capture != nil {
		capture.report.ErrorKind = "setup"
		stderrR, stderrW, err = os.Pipe()
	}
	if capture == nil || err != nil {
		captureIncomplete = capture != nil
		stderrW, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	if err != nil {
		return 0, err
	}
	defer stderrW.Close()
	if stderrR != nil {
		defer stderrR.Close()
	}

	process, err := startSuspended(cmd, job, stdinR, stdoutW, stderrW)
	if err != nil {
		return 0, fmt.Errorf("无法启动 Agent 进程: %w", err)
	}
	defer windows.CloseHandle(process.Process)
	defer windows.CloseHandle(process.Thread)
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	if err := started(); err != nil {
		return 0, errors.Join(err, stopProcessTree(job))
	}
	if err := ctx.Err(); err != nil {
		return 0, errors.Join(err, stopProcessTree(job))
	}
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		return 0, errors.Join(err, stopProcessTree(job))
	}
	var stderrDone chan error
	if capture != nil {
		stdoutLog, stderrLog = capture.openStreams()
		defer func() { capture.finishStreams(stdoutLog, stderrLog, captureIncomplete) }()
		if stderrR != nil {
			stderrDone = make(chan error, 1)
			go func() {
				_, err := io.Copy(stderrLog, stderrR)
				stderrDone <- err
			}()
		} else {
			stderrLog.err = true
		}
	}

	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	input := make(chan error, 1)
	go func() {
		_, err := io.WriteString(stdinW, prompt)
		stdinW.Close()
		input <- err
	}()
	output := make(chan error, 1)
	go func() {
		reader := capturedReader(stdoutR, stdoutLog)
		err := read(reader)
		if err != nil {
			cancel()
		}
		if stdoutLog != nil {
			_, drainErr := io.Copy(io.Discard, reader)
			stdoutLog.err = stdoutLog.err || drainErr != nil
		}
		output <- err
	}()
	waitErr := waitProcess(processCtx, process.Process)
	// 主进程退出或被取消时，先处理仍持有本轮资源的子进程，再收回管道。
	treeErr := stopProcessTree(job)
	var exitCode uint32
	exitErr := windows.GetExitCodeProcess(process.Process, &exitCode)
	if treeErr != nil {
		// 无法确认子进程退出时关闭本端管道，避免遗留进程占用管道阻塞设备。
		_ = stdoutR.Close()
		_ = stdinW.Close()
		if stderrR != nil {
			_ = stderrR.Close()
		}
	}
	outputErr := <-output
	inputErr := <-input
	if stderrDone != nil {
		stderrErr := <-stderrDone
		captureIncomplete = captureIncomplete || stderrErr != nil
	}
	if capture != nil {
		capture.report.ErrorKind = ""
		if exitErr == nil && treeErr == nil {
			capture.report.ExitCode = &exitCode
		}
		switch {
		case treeErr != nil || ctx.Err() != nil:
			capture.report.ErrorKind = "interrupted"
		case waitErr != nil || exitErr != nil || outputErr != nil || inputErr != nil:
			capture.report.ErrorKind = "output"
		case exitCode != 0:
			capture.report.ErrorKind = "native_exit"
		}
		captureIncomplete = captureIncomplete || treeErr != nil
	}
	if treeErr != nil {
		return exitCode, treeErr
	}
	if err := ctx.Err(); err != nil {
		return exitCode, err
	}
	return exitCode, errors.Join(waitErr, exitErr, outputErr, inputErr)
}

func waitProcess(ctx context.Context, process windows.Handle) error {
	for ctx.Err() == nil {
		state, err := windows.WaitForSingleObject(process, 100)
		if err != nil {
			return fmt.Errorf("无法读取 Agent 进程退出状态：%w", err)
		}
		if state == windows.WAIT_OBJECT_0 {
			return nil
		}
		if state != uint32(windows.WAIT_TIMEOUT) {
			return fmt.Errorf("无法确认 Agent 退出状态 (%d)", state)
		}
	}
	return nil
}

// stopProcessTree 只终止本轮持有的 Job Object，并等待确认进程树退出。
func stopProcessTree(job windows.Handle) error {
	// 先阻止 Job 加入新进程，以此刻的成员句柄核实实际退出。
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	_, limitErr := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	var members []windows.Handle
	var listErr error
	if limitErr == nil {
		members, listErr = openJobMembers(job)
	}
	for _, member := range members {
		defer windows.CloseHandle(member)
	}
	terminateErr := windows.TerminateJobObject(job, 1)
	if err := errors.Join(limitErr, listErr, terminateErr); err != nil {
		return errors.Join(ErrProcessExitUnconfirmed, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, member := range members {
		timeout := uint32(max(0, time.Until(deadline).Milliseconds()))
		state, err := windows.WaitForSingleObject(member, timeout)
		if err != nil || state != windows.WAIT_OBJECT_0 {
			return errors.Join(ErrProcessExitUnconfirmed, err)
		}
	}
	return nil
}

var isProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func openJobMembers(job windows.Handle) ([]windows.Handle, error) {
	// JOBOBJECT_BASIC_PROCESS_ID_LIST 的前两个字段为 DWORD，后续进程 ID 使用指针宽度。
	for capacity := 16; capacity <= 65536; capacity *= 2 {
		type processListHeader struct {
			Assigned, Listed uint32
		}
		var header processListHeader
		headerSlots := int(unsafe.Sizeof(header) / unsafe.Sizeof(uintptr(0)))
		buffer := make([]uintptr, headerSlots+capacity)
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&buffer[0])), uint32(len(buffer))*uint32(unsafe.Sizeof(buffer[0])), nil)
		if errors.Is(err, windows.ERROR_MORE_DATA) {
			continue
		}
		if err != nil {
			return nil, err
		}
		header = *(*processListHeader)(unsafe.Pointer(&buffer[0]))
		if header.Listed > uint32(capacity) || header.Assigned != header.Listed {
			continue
		}
		members := make([]windows.Handle, 0, header.Listed)
		for _, pid := range buffer[headerSlots : headerSlots+int(header.Listed)] {
			process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				continue // 进程已退出，进程 ID 已从内核表中移除。
			}
			if err != nil {
				return members, err
			}
			var belongs uint32
			ok, _, memberErr := isProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&belongs)))
			if ok == 0 || belongs == 0 {
				_ = windows.CloseHandle(process)
				if ok == 0 {
					return members, memberErr
				}
				continue // 已退出进程的 ID 已被本轮 Job 外的进程复用。
			}
			members = append(members, process)
		}
		return members, nil
	}
	return nil, errors.New("无法枚举本轮 Agent 进程树")
}

func startSuspended(cmd *exec.Cmd, job windows.Handle, input, output, log *os.File) (windows.ProcessInformation, error) {
	var info windows.ProcessInformation
	executable, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		return info, err
	}
	command, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(cmd.Args))
	if err != nil {
		return info, err
	}
	directory, err := windows.UTF16PtrFromString(cmd.Dir)
	if err != nil {
		return info, err
	}
	environment := utf16.Encode([]rune(strings.Join(cmd.Env, "\x00") + "\x00\x00"))

	handleList := make([]windows.Handle, 3)
	current := windows.CurrentProcess()
	for index, file := range []*os.File{input, output, log} {
		err := windows.DuplicateHandle(current, windows.Handle(file.Fd()), current, &handleList[index], 0, true, windows.DUPLICATE_SAME_ACCESS)
		if err != nil {
			return info, err
		}
		defer windows.CloseHandle(handleList[index])
	}

	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return info, err
	}
	defer attributes.Delete()
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handleList[0]), uintptr(len(handleList))*unsafe.Sizeof(handleList[0])); err != nil {
		return info, err
	}
	if err := attributes.Update(processJobList, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
		return info, err
	}
	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:     windows.STARTF_USESTDHANDLES,
			StdInput:  handleList[0],
			StdOutput: handleList[1],
			StdErr:    handleList[2],
		},
		ProcThreadAttributeList: attributes.List(),
	}
	flags := uint32(createNoWindow | windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	err = windows.CreateProcess(executable, command, nil, nil, true, flags, &environment[0], directory, &startup.StartupInfo, &info)
	return info, err
}
