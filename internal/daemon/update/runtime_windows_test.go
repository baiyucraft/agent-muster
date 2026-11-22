//go:build windows

package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/baiyucraft/agent-muster/internal/deviceupdate"
)

// TestWindowsUpdateRoundTrip 用隔离安装目录运行真实 Windows 进程，验证两个程序一起更新或回滚。
func TestWindowsUpdateRoundTrip(t *testing.T) {
	if os.Getenv("MUSTER_TEST_WINDOWS_UPDATE") != "1" {
		t.Skip("set MUSTER_TEST_WINDOWS_UPDATE=1 to build isolated Windows processes")
	}
	oldApp := buildTestDevice(t, "0.2.0")
	newApp := buildTestDevice(t, "0.2.1")
	for _, test := range []struct {
		name        string
		newApp      []byte
		wantStatus  string
		wantVersion string
		wantCLI     []byte
	}{
		{name: "installs and starts", newApp: newApp, wantStatus: deviceupdate.StatusSucceeded, wantVersion: "0.2.1", wantCLI: []byte("new companion")},
		{name: "rolls back corrupt desktop", newApp: []byte("signed but not a Windows executable"), wantStatus: deviceupdate.StatusRolledBack, wantVersion: "0.2.0", wantCLI: []byte("old companion")},
	} {
		t.Run(test.name, func(t *testing.T) {
			install := t.TempDir()
			public, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string][]byte{
				AppName: oldApp, CLIName: []byte("old companion"), PublicKeyName: []byte(base64.StdEncoding.EncodeToString(public)),
				"local-state": []byte("keep me"),
			} {
				if err := os.WriteFile(filepath.Join(install, name), value, 0600); err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				if _, err := os.Stat(filepath.Join(install, "started")); err == nil {
					_ = os.WriteFile(filepath.Join(install, "stop"), nil, 0600)
					waitFor(t, func() bool {
						if unlock, err := lock(filepath.Join(install, updateDir), runningLockByte, true); err == nil {
							unlock()
							return true
						}
						return false
					})
				}
			}()
			contents := map[string][]byte{AppName: test.newApp, CLIName: []byte("new companion")}
			archive := encodeArchive(contents)
			server := httptest.NewUnstartedServer(nil)
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/offer":
					manifest, _ := manifestFor("0.2.1", server.URL+"/release.zip", contents)
					manifest.Platform = platform()
					offer, err := Sign(manifest, private)
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					json.NewEncoder(w).Encode(offer)
				case "/release.zip":
					w.Write(archive)
				default:
					http.NotFound(w, r)
				}
			})
			server.Start()
			defer server.Close()

			cmd := exec.CommandContext(t.Context(), filepath.Join(install, AppName), "-manifest="+server.URL+"/offer")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("start update: %v: %s", err, output)
			}
			var result Result
			var started []byte
			deadline := time.Now().Add(45 * time.Second)
			for time.Now().Before(deadline) {
				resultErr := readRecord(resultPath(install, "test"), &result)
				var startedErr error
				started, startedErr = os.ReadFile(filepath.Join(install, "started"))
				if resultErr == nil && startedErr == nil {
					break
				}
				if resultErr != nil && !errors.Is(resultErr, os.ErrNotExist) {
					t.Fatal(resultErr)
				}
				time.Sleep(100 * time.Millisecond)
			}
			if result.State != test.wantStatus || result.AssignmentID != "upd-0123456789abcdef0123456789abcdef" ||
				string(started) != test.wantVersion {
				logFiles, _ := filepath.Glob(filepath.Join(install, updateDir, "release-*", "helper.log"))
				for _, filename := range logFiles {
					data, _ := os.ReadFile(filename)
					t.Logf("update helper: %s", data)
				}
				t.Fatalf("result %+v and started %q, want status %s and started %s", result, started, test.wantStatus, test.wantVersion)
			}
			for name, expected := range map[string][]byte{CLIName: test.wantCLI, "local-state": []byte("keep me")} {
				actual, err := os.ReadFile(filepath.Join(install, name))
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("%s = %q, %v", name, actual, err)
				}
			}
		})
	}
}

func buildTestDevice(t *testing.T, version string) []byte {
	t.Helper()
	binary := filepath.Join(t.TempDir(), AppName)
	cmd := exec.CommandContext(t.Context(), "go", "build",
		"-ldflags", "-X github.com/baiyucraft/agent-muster/internal/daemon/update.Version="+version,
		"-o", binary, "./testdata/device")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s device: %v: %s", version, err, output)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("device did not exit after the test")
}
