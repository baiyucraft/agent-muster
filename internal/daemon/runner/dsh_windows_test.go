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

func TestJobCancellationWaitsForNativeProcessTree(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestJobProcessFixture$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(cmd.Environ(), "MUSTER_JOB_FIXTURE=parent")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var child windows.Handle
	_, err = runInJob(ctx, cmd, "", func() error { return nil }, func(reader io.Reader) error {
		pidText, err := bufio.NewReader(reader).ReadString('\n')
		if err != nil {
			return err
		}
		pid, err := strconv.ParseUint(strings.TrimSpace(pidText), 10, 32)
		if err != nil {
			return err
		}
		child, err = windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		cancel()
		return err
	})
	if child == 0 {
		t.Fatalf("未取得子进程句柄：%v", err)
	}
	defer windows.CloseHandle(child)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("进程树取消返回 %v", err)
	}
	state, waitErr := windows.WaitForSingleObject(child, 0)
	if waitErr != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("运行器返回时子进程仍未退出，状态 %d，错误 %v", state, waitErr)
	}
}

func TestJobProcessFixture(t *testing.T) {
	switch os.Getenv("MUSTER_JOB_FIXTURE") {
	case "parent":
		// 子进程不继承本轮的标准流，单凭管道 EOF 无法证明它已退出。
		child := exec.Command(os.Args[0], "-test.run=^TestJobProcessFixture$")
		child.Env = append(child.Environ(), "MUSTER_JOB_FIXTURE=child")
		if err := child.Start(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(20 * time.Second)
	case "child":
		time.Sleep(20 * time.Second)
	}
}
