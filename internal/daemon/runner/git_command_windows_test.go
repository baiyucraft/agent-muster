package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectCommandUsesDirectoryAndLiteralArguments(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	argument := `参数名 "带引号" & %TEMP% | (空格) \`
	t.Setenv("MUSTER_RUN_CAPABILITY", "fixture-capability")
	var stdout, stderr bytes.Buffer
	code, err := RunProjectCommandStreams(ctx, dir, executable, []string{"-test.run=^TestProjectCommandChild$", "--", argument}, &stdout, &stderr)
	if err != nil || code != 7 {
		t.Fatalf("进程退出码 %d，错误 %v，输出 %q，错误流 %q", code, err, stdout.String(), stderr.String())
	}
	var output struct {
		Directory string
		Argument  string
		Input     string
		CapSet    bool
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Clean(dir), filepath.Clean(output.Directory)) || output.Argument != argument || output.Input != "" || output.CapSet {
		t.Fatalf("项目命令输出不符预期：%+v", output)
	}
	if stderr.String() != "native stderr\n" {
		t.Fatalf("错误流输出为 %q", stderr.String())
	}
}

func TestProjectCommandCapturesGitOutput(t *testing.T) {
	if _, err := exec.LookPath("git.exe"); err != nil {
		t.Skip("本机缺少 Git CLI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var output bytes.Buffer
	code, err := RunProjectCommand(ctx, t.TempDir(), "git.exe", []string{"--version"}, &output)
	if err != nil || code != 0 || !strings.HasPrefix(output.String(), "git version ") {
		t.Fatalf("Git 退出码 %d，错误 %v，输出 %q", code, err, output.String())
	}
}

func TestProjectCommandPropagatesStderrWriterError(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	want := errors.New("无法写入测试日志")
	_, err = RunProjectCommandStreams(ctx, t.TempDir(), executable, []string{"-test.run=^TestProjectCommandChild$", "--", "wait"}, nil, projectWriterError{want})
	if !errors.Is(err, want) {
		t.Fatalf("项目命令错误为 %v，期望 %v", err, want)
	}
}

type projectWriterError struct{ err error }

func (w projectWriterError) Write([]byte) (int, error) { return 0, w.err }

func TestProjectCommandChild(t *testing.T) {
	if len(os.Args) != 4 || os.Args[2] != "--" {
		return
	}
	fmt.Fprint(os.Stderr, "native stderr\n")
	if os.Args[3] == "wait" {
		time.Sleep(20 * time.Second)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	_, capSet := os.LookupEnv("MUSTER_RUN_CAPABILITY")
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"Directory": dir, "Argument": os.Args[3], "Input": string(input), "CapSet": capSet}); err != nil {
		t.Fatal(err)
	}
	os.Exit(7)
}
