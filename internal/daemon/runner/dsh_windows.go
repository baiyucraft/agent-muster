package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
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
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	defer stderr.Close()

	process, err := startSuspended(cmd, job, stdinR, stdoutW, stderr)
	if err != nil {
		return 0, fmt.Errorf("无法启动 Agent 进程: %w", err)
	}
	defer windows.CloseHandle(process.Process)
	defer windows.CloseHandle(process.Thread)
	stdinR.Close()
	stdoutW.Close()
	if err := started(); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		return 0, err
	}

	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	killed := make(chan struct{})
	stopKill := context.AfterFunc(processCtx, func() {
		_ = windows.TerminateJobObject(job, 1)
		close(killed)
	})
	defer func() {
		if !stopKill() {
			<-killed
		}
	}()
	input := make(chan error, 1)
	go func() {
		_, err := io.WriteString(stdinW, prompt)
		stdinW.Close()
		input <- err
	}()
	output := make(chan error, 1)
	go func() {
		err := read(stdoutR)
		output <- err
		if err != nil {
			cancel()
		}
	}()
	state, waitErr := windows.WaitForSingleObject(process.Process, windows.INFINITE)
	if waitErr != nil || state != windows.WAIT_OBJECT_0 {
		cancel()
	}
	var exitCode uint32
	exitErr := windows.GetExitCodeProcess(process.Process, &exitCode)
	// dsh has exited. End any remaining descendants so inherited pipe handles
	// cannot leave the readers and writers blocked.
	_ = windows.TerminateJobObject(job, 1)
	outputErr := <-output
	inputErr := <-input
	if err := ctx.Err(); err != nil {
		return exitCode, err
	}
	if waitErr != nil || state != windows.WAIT_OBJECT_0 {
		return exitCode, fmt.Errorf("无法确认 Agent 退出状态 (%d): %w", state, waitErr)
	}
	return exitCode, errors.Join(exitErr, outputErr, inputErr)
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
