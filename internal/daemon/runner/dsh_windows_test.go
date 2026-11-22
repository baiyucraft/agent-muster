package runner

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
