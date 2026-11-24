package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestJobRunsWithBoundDirectoryAndPromptOnStdin(t *testing.T) {
	node, err := exec.LookPath("node.exe")
	if err != nil {
		t.Skip("Node.js is unavailable")
	}
	const input = "识别文件名 %TEMP% & $(echo unexpected)"
	path := t.TempDir()
	cmd := exec.Command(node, "-e", "process.stdin.setEncoding('utf8'); process.stdin.pipe(process.stdout); console.log(process.cwd())")
	cmd.Dir = path
	cmd.Env = cmd.Environ()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout string
	var started bool
	status, err := runInJob(ctx, cmd, input, func() error {
		started = true
		return nil
	}, func(reader io.Reader) error {
		data, err := io.ReadAll(reader)
		stdout = string(data)
		return err
	})
	if err != nil || status != 0 {
		t.Fatalf("status=%d err=%v output=%q", status, err, stdout)
	}
	line, prompt, ok := strings.Cut(stdout, "\n")
	if !started || !ok || !strings.EqualFold(filepath.Clean(strings.TrimSpace(line)), filepath.Clean(path)) || prompt != input {
		t.Fatalf("started=%v output=%q", started, stdout)
	}
}

func TestJobWaitsForNativeProcessTree(t *testing.T) {
	for _, mode := range []string{"exit", "cancel"} {
		t.Run(mode, func(t *testing.T) { verifyJobExit(t, mode) })
	}
}

func verifyJobExit(t *testing.T, mode string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestJobProcessFixture$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(cmd.Environ(), "MUSTER_JOB_FIXTURE=parent-"+mode)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var child windows.Handle
	code, err := runInJob(ctx, cmd, "", func() error { return nil }, func(reader io.Reader) error {
		pidText, err := bufio.NewReader(reader).ReadString('\n')
		if err != nil {
			return err
		}
		pid, err := strconv.ParseUint(strings.TrimSpace(pidText), 10, 32)
		if err != nil {
			return err
		}
		child, err = windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			return err
		}
		if mode == "cancel" {
			cancel()
			return nil
		}
		// 先持有子进程的观察句柄，再允许主进程正常退出。
		return os.WriteFile(filepath.Join(cmd.Dir, "child-observed"), nil, 0600)
	})
	if child == 0 {
		t.Fatalf("未取得子进程句柄：%v", err)
	}
	defer windows.CloseHandle(child)
	if mode == "cancel" {
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrProcessExitUnconfirmed) {
			t.Fatalf("进程树取消返回 %v", err)
		}
	} else if code != 0 || err != nil {
		t.Fatalf("主进程正常退出时返回退出码 %d 和错误 %v", code, err)
	}
	state, waitErr := windows.WaitForSingleObject(child, 0)
	if waitErr != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("运行器返回时子进程仍未退出，状态 %d，错误 %v", state, waitErr)
	}
}

func TestJobCapturesNativeStreamsWhenParserStopsEarly(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestJobProcessFixture$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(cmd.Environ(), "MUSTER_JOB_FIXTURE=diagnostics")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	ctx, report := WithDiagnostics(ctx, dir)
	code, err := runInJob(ctx, cmd, "", func() error { return nil }, func(r io.Reader) error {
		_, err := io.ReadFull(r, make([]byte, 3))
		return err
	})
	if code != 7 || err != nil {
		t.Fatalf("进程退出码 %d，错误 %v", code, err)
	}
	out, errOut := os.ReadDir(dir)
	if errOut != nil || len(out) != 1 {
		t.Fatalf("运行日志目录 %v，错误 %v", out, errOut)
	}
	const stderr = "设备本机 stderr\n"
	stdout := strings.Repeat("本机 stdout\n", 1024)
	for name, want := range map[string]string{"stdout.log": stdout, "stderr.log": stderr} {
		body, err := os.ReadFile(filepath.Join(dir, out[0].Name(), name))
		if err != nil || string(body) != want {
			t.Fatalf("%s 保存了 %d 字节，预期 %d，错误 %v", name, len(body), len(want), err)
		}
	}
	if got := report(); got == nil || got.ExitCode == nil || *got.ExitCode != 7 ||
		got.CaptureIncomplete || got.StdoutBytes == nil || *got.StdoutBytes != int64(len(stdout)) ||
		got.StderrBytes == nil || *got.StderrBytes != int64(len(stderr)) || got.ErrorKind != "native_exit" {
		t.Fatalf("运行诊断不完整：%+v", got)
	}
}

func TestJobProcessFixture(t *testing.T) {
	switch os.Getenv("MUSTER_JOB_FIXTURE") {
	case "diagnostics":
		fmt.Fprint(os.Stderr, "设备本机 stderr\n")
		fmt.Fprint(os.Stdout, strings.Repeat("本机 stdout\n", 1024))
		os.Exit(7)
	case "parent-exit", "parent-cancel":
		// 子进程不继承本轮的标准流，单凭管道 EOF 无法证明它已退出。
		child := exec.Command(os.Args[0], "-test.run=^TestJobProcessFixture$")
		child.Env = append(child.Environ(), "MUSTER_JOB_FIXTURE=child")
		if err := child.Start(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println(child.Process.Pid)
		if os.Getenv("MUSTER_JOB_FIXTURE") == "parent-exit" {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat("child-observed"); err == nil {
					os.Exit(0)
				}
				time.Sleep(10 * time.Millisecond)
			}
			os.Exit(1)
		}
		time.Sleep(20 * time.Second)
	case "child":
		time.Sleep(20 * time.Second)
	}
}
