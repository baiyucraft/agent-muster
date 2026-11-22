package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProbeCommandStartsNativeExecutableWithoutConsole(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := probeCommand(context.Background(), executable, "--version")
	if err != nil {
		t.Fatal(err)
	}
	assertProbeWithoutConsole(t, cmd)
}

func TestProbeCommandRunsNodeScriptFromCommandShim(t *testing.T) {
	_, err := exec.LookPath("node.exe")
	if err != nil {
		_, err = exec.LookPath("node")
	}
	if err != nil {
		t.Skip("node is not installed")
	}
	directory := t.TempDir()
	shimDirectory := filepath.Join(directory, "bin")
	if err := os.MkdirAll(filepath.Join(shimDirectory, "node_modules", "test-agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimDirectory, "node.exe"), []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(shimDirectory, "node.exe")); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(shimDirectory, "node_modules", "test-agent", "bin.js")
	if err := os.WriteFile(script, []byte(`console.log(process.argv.slice(2).join(" "));`), 0o600); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(shimDirectory, "codex.cmd")
	contents := []byte("@ECHO off\r\n\"%dp0%\\node_modules\\test-agent\\bin.js\" %*\r\n")
	if err := os.WriteFile(shim, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd, err := probeCommand(context.Background(), shim, "--version")
	if err != nil {
		t.Fatal(err)
	}
	assertProbeWithoutConsole(t, cmd)
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "--version\r\n" && string(output) != "--version\n" {
		t.Fatalf("node output = %q", output)
	}
}

func assertProbeWithoutConsole(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	options := cmd.SysProcAttr
	if options == nil || !options.HideWindow || options.CreationFlags&createNoWindow == 0 {
		t.Fatal("probe must start without showing or allocating a console window")
	}
}

func TestProbeCommandRejectsShimScriptOutsideDirectory(t *testing.T) {
	directory := t.TempDir()
	shim := filepath.Join(directory, "codex.cmd")
	if err := os.WriteFile(shim, []byte("\"%dp0%\\..\\outside.js\" %*\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := probeCommand(context.Background(), shim, "--version"); err == nil {
		t.Fatal("accepted script outside shim directory")
	}
}
