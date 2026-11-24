package work

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"golang.org/x/sys/windows"
)

const (
	crashTestRun     = "run-0705b167-88ae-4af3-a0dc-4b105d931463"
	crashTestSession = "1e076da3-4fcb-4cd3-b963-4ba4258669dd"
	crashTestToken   = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func TestMain(m *testing.M) {
	args := os.Args[1:]
	if handled, err := runner.HandleProcessProofHelper(args); handled {
		exitCrashFixture(err)
	}
	if len(args) > 0 && strings.EqualFold(filepath.Base(os.Args[0]), "codex.exe") {
		switch args[0] {
		case "exec":
			exitCrashFixture(runCrashFixtureAgent())
		case "--test-crash-child":
			exitCrashFixture(pauseCrashFixture("child.pid"))
		}
	}
	os.Exit(m.Run())
}

func exitCrashFixture(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runCrashFixtureAgent() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	child := exec.Command(exe, "--test-crash-child")
	child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := child.Start(); err != nil {
		return err
	}
	defer child.Wait()
	fmt.Printf("{\"type\":\"thread.started\",\"thread_id\":%q}\n{\"type\":\"turn.started\"}\n", crashTestSession)
	return pauseCrashFixture("agent.pid")
}

func pauseCrashFixture(file string) error {
	if err := os.WriteFile(file, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return err
	}
	time.Sleep(30 * time.Second)
	return nil
}

func TestCrashRecoveryWorkerFixture(t *testing.T) {
	state := os.Getenv("MUSTER_TEST_CRASH_STATE")
	if state == "" {
		return
	}
	endpoint := os.Getenv("MUSTER_TEST_CRASH_HUB")
	client, err := control.NewClient(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	identity := control.Identity{HubURL: endpoint, DeviceID: "d-00000000000000000000000000000000", Token: crashTestToken}
	peer := &collaboration{worker: worker, client: client, identity: identity}
	onError := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	if err := worker.run(ctx, client, identity, onError, peer, nil, executeAgent); err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
}

func TestWindowsCrashRecoveryConfirmsExitBeforeReportingAndClaiming(t *testing.T) {
	bin, project, state := t.TempDir(), t.TempDir(), t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex.exe"), testBinary, 0700); err != nil {
		t.Fatal(err)
	}
	seed, err := Open(state)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer seed.Close()
		dir, err := seed.RegisterDirectory(t.Context(), project)
		if err != nil {
			t.Fatal(err)
		}
		if err := seed.saveClaim(t.Context(), control.Run{
			ID: crashTestRun, TaskID: "task-crash", AgentKind: "codex", DirectoryID: dir.ID,
			Generation: 7, Lease: "fixture-lease", Prompt: "fixture",
			LeaseUntil: time.Now().Add(time.Minute).Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatal(err)
		}
	}()

	var mu sync.Mutex
	var members []windows.Handle
	var interrupted []control.RunEvent
	var premature, claimed, sessionSeen bool
	var fixtureFailure string
	firstInterrupted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Header.Get("Authorization") != "Bearer "+crashTestToken:
			w.WriteHeader(http.StatusUnauthorized)
			return
		case strings.HasSuffix(r.URL.Path, "/runs/claim"):
			premature = premature || len(interrupted) < 2 || crashMembersAlive(members)
			claimed = true
		case strings.Contains(r.URL.Path, "/events/"):
			var event control.RunEvent
			if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch event.Kind {
			case "failed":
				fixtureFailure = event.Text
			case "session":
				sessionSeen = event.SessionID == crashTestSession
			case "interrupted":
				premature = premature || crashMembersAlive(members) || !strings.HasSuffix(r.URL.Path, "/events/3")
				interrupted = append(interrupted, event)
				if len(interrupted) == 1 {
					close(firstInterrupted)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	first := startCrashFixtureWorker(t, exe, state, server.URL, bin)
	awaitCrashFixture(t, "原会话上报", func() bool {
		mu.Lock()
		defer mu.Unlock()
		if fixtureFailure != "" {
			t.Fatalf("替身 CLI 启动失败：%s", fixtureFailure)
		}
		return sessionSeen
	})
	var observed []windows.Handle
	for _, name := range []string{"agent.pid", "child.pid"} {
		var process windows.Handle
		awaitCrashFixture(t, name, func() bool {
			raw, err := os.ReadFile(filepath.Join(project, name))
			if err != nil {
				return false
			}
			pid, err := strconv.ParseUint(string(raw), 10, 32)
			if err != nil {
				return false
			}
			process, err = windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
			return err == nil
		})
		defer windows.CloseHandle(process)
		observed = append(observed, process)
		if !crashMembersAlive([]windows.Handle{process}) {
			t.Fatalf("崩溃前 %s 已退出", name)
		}
	}
	mu.Lock()
	members = observed
	mu.Unlock()
	first()
	second := startCrashFixtureWorker(t, exe, state, server.URL, bin)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if t.Failed() {
			t.Logf("中断回执次数=%d，领取=%v，提前操作=%v", len(interrupted), claimed, premature)
		}
	})
	select {
	case <-firstInterrupted:
	case <-time.After(8 * time.Second):
		t.Fatal("等待首条中断回执超时")
	}
	second()
	third := startCrashFixtureWorker(t, exe, state, server.URL, bin)
	awaitCrashFixture(t, "退出确认后的回执重试及领取", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return claimed
	})
	third()
	mu.Lock()
	if premature || len(interrupted) != 2 || !reflect.DeepEqual(interrupted[0], interrupted[1]) ||
		interrupted[0].Generation != 7 || interrupted[0].Lease != "fixture-lease" ||
		interrupted[0].SessionID != crashTestSession {
		mu.Unlock()
		t.Fatal("恢复顺序或原运行的回执、会话和租约不符合预期")
	}
	mu.Unlock()
	reopened, err := Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	status, err := reopened.LatestRun(t.Context())
	if err != nil || status == nil || status.State != "interrupted" || status.Pending {
		t.Fatalf("Hub 确认后本机仍有待提交回执：%+v，%v", status, err)
	}
}

func crashMembersAlive(members []windows.Handle) bool {
	for _, process := range members {
		if state, err := windows.WaitForSingleObject(process, 0); err != nil || state != windows.WAIT_OBJECT_0 {
			return true
		}
	}
	return false
}

func startCrashFixtureWorker(t *testing.T, exe, state, endpoint, bin string) func() {
	t.Helper()
	cmd := exec.Command(exe, "-test.run=^TestCrashRecoveryWorkerFixture$")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.Env = append(os.Environ(), "MUSTER_TEST_CRASH_STATE="+state, "MUSTER_TEST_CRASH_HUB="+endpoint,
		"PATH="+bin+";"+os.Getenv("PATH"))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	t.Cleanup(stop)
	return stop
}

func awaitCrashFixture(t *testing.T, name string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", name)
}
