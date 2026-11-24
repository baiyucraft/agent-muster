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

func TestMain(m *testing.M) {
	if handled, err := HandleProcessProofHelper(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const testProofName = "exit-0123456789abcdeffedcba9876543210.proof"

func TestProcessProofRequiresCompleteMatchingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), testProofName)
	if confirmed, err := ProcessExitConfirmed(path); confirmed || err != nil {
		t.Fatalf("未写入的证据返回 %v, %v", confirmed, err)
	}
	if err := os.WriteFile(path, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := ProcessExitConfirmed(path); confirmed || err == nil {
		t.Fatalf("不完整的证据返回 %v, %v", confirmed, err)
	}
	if err := writeProcessProof(path); err == nil {
		t.Fatal("覆盖了既有证据")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := writeProcessProof(path); err != nil {
			t.Fatal(err)
		}
	}
	if confirmed, err := ProcessExitConfirmed(path); !confirmed || err != nil {
		t.Fatalf("完整的证据返回 %v, %v", confirmed, err)
	}
	other := filepath.Join(filepath.Dir(path), "exit-0123456789abcdeffedcba9876543211.proof")
	if err := os.Rename(path, other); err != nil {
		t.Fatal(err)
	}
	if confirmed, err := ProcessExitConfirmed(other); confirmed || err == nil {
		t.Fatalf("错误文件名的证据返回 %v, %v", confirmed, err)
	}
}

func TestProtectedJobUsesPersistentPermission(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), testProofName)
			armErr := errors.New("模拟持久化失败")
			var armed, started bool
			ctx := WithProcessProof(t.Context(), func() (string, func() error, error) {
				return path, func() error {
					armed = true
					if !allow {
						return armErr
					}
					return nil
				}, nil
			})
			cmd := proofFixtureCommand(t, "exit")
			code, err := runInJob(ctx, cmd, "", func() error {
				started = true
				if !armed {
					return errors.New("持久化执行许可前已启动")
				}
				return nil
			}, func(r io.Reader) error {
				_, err := io.Copy(io.Discard, r)
				return err
			}, nil)
			if !armed || started != allow || (allow && (code != 0 || err != nil)) || (!allow && !errors.Is(err, armErr)) {
				t.Fatalf("armed=%t started=%t code=%d err=%v", armed, started, code, err)
			}
			if confirmed, err := ProcessExitConfirmed(path); !confirmed || err != nil {
				t.Fatalf("本轮退出未留下证据：%v, %v", confirmed, err)
			}
		})
	}
}

func TestProcessGuardianAfterOwnerExit(t *testing.T) {
	for _, keepGuardian := range []bool{true, false} {
		t.Run("guardian-survives="+strconv.FormatBool(keepGuardian), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, testProofName)
			owner := proofFixtureCommand(t, "owner")
			owner.Dir = dir
			stdout, err := owner.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			owner.Stderr = os.Stderr
			if err := owner.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = owner.Process.Kill()
				_ = owner.Wait()
			})
			type members struct{ guardian, child uint32 }
			ready := make(chan members, 1)
			go func() {
				var pair members
				_, _ = fmt.Fscan(bufio.NewReader(stdout), &pair.guardian, &pair.child)
				ready <- pair
			}()
			var pair members
			select {
			case pair = <-ready:
			case <-time.After(10 * time.Second):
				t.Fatal("没有取得辅助进程或 CLI 子进程 ID")
			}
			guardian := proofFixtureProcess(t, pair.guardian)
			child := proofFixtureProcess(t, pair.child)
			if !keepGuardian {
				if err := windows.TerminateProcess(guardian, 1); err != nil {
					t.Fatal(err)
				}
				if state, err := windows.WaitForSingleObject(guardian, 5000); state != windows.WAIT_OBJECT_0 || err != nil {
					t.Fatalf("辅助进程未退出：%d, %v", state, err)
				}
			}
			if state, err := windows.WaitForSingleObject(child, 0); state != uint32(windows.WAIT_TIMEOUT) || err != nil {
				t.Fatalf("终止设备进程前 CLI 子进程必须仍在执行：%d, %v", state, err)
			}
			if err := owner.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = owner.Wait()
			if state, err := windows.WaitForSingleObject(guardian, 8000); state != windows.WAIT_OBJECT_0 || err != nil {
				t.Fatalf("终止设备后辅助进程未结束：%d, %v", state, err)
			}
			if state, err := windows.WaitForSingleObject(child, 8000); state != windows.WAIT_OBJECT_0 || err != nil {
				t.Fatalf("Job 内的 CLI 子进程未结束：%d, %v", state, err)
			}
			if confirmed, err := ProcessExitConfirmed(path); confirmed != keepGuardian || err != nil {
				t.Fatalf("退出证据=%t，预期 %t，错误 %v", confirmed, keepGuardian, err)
			}
		})
	}
}

func proofFixtureCommand(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestProcessProofFixture$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(cmd.Environ(), "MUSTER_PROOF_FIXTURE="+mode)
	return cmd
}

func proofFixtureProcess(t *testing.T, pid uint32) windows.Handle {
	t.Helper()
	if pid == 0 {
		t.Fatal("没有取得实际进程 ID")
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	return handle
}

func TestProcessProofFixture(t *testing.T) {
	switch os.Getenv("MUSTER_PROOF_FIXTURE") {
	case "exit":
		os.Exit(0)
	case "owner":
		proofOwnerFixture(t)
	case "cli":
		child := proofFixtureCommand(t, "child")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(30 * time.Second)
	case "child":
		time.Sleep(30 * time.Second)
	}
}

func proofOwnerFixture(t *testing.T) {
	t.Helper()
	job, err := createProcessJob()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(job)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	guardian, err := startProcessGuardian(t.Context(), job, filepath.Join(dir, testProofName))
	if err != nil {
		t.Fatal(err)
	}
	defer guardian.finish(false)
	guardianPID, err := windows.GetProcessId(guardian.process)
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	process, err := startSuspended(proofFixtureCommand(t, "cli"), job, input, write, input)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process.Process)
	_, err = windows.ResumeThread(process.Thread)
	windows.CloseHandle(process.Thread)
	_ = write.Close()
	if err != nil {
		t.Fatal(err)
	}
	child, err := bufio.NewReader(read).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(guardianPID, strings.TrimSpace(child))
	if err := waitProcess(context.Background(), process.Process); err != nil {
		t.Fatal(err)
	}
}
