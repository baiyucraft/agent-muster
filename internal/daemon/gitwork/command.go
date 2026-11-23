package gitwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
)

func git(ctx context.Context, dir string, args ...string) (string, error) {
	output, code, err := runGit(ctx, dir, args...)
	if err == nil && code != 0 {
		err = fmt.Errorf("Git 命令退出码 %d", code)
	}
	return output, err
}

func runGit(ctx context.Context, dir string, args ...string) (string, uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var output bytes.Buffer
	bounded := limitedOutput{destination: &output, left: 64 << 10}
	// Git 输出可能携带设备路径或远端凭据。这里只向调用方返回有上限的 stdout。
	code, err := runner.RunProjectCommandStreams(ctx, dir, "git", append([]string{"--no-replace-objects"}, args...), &bounded, nil)
	if bounded.truncated {
		err = errors.Join(err, errors.New("Git 命令输出超过允许范围"))
	}
	return output.String(), code, err
}

type limitedOutput struct {
	destination io.Writer
	left        int
	truncated   bool
}

// Write 达到记录上限后继续排空子进程输出，避免阻塞执行。
func (out *limitedOutput) Write(data []byte) (int, error) {
	count := min(len(data), out.left)
	if count != 0 {
		if written, err := out.destination.Write(data[:count]); err != nil || written != count {
			return written, errors.Join(err, io.ErrShortWrite)
		}
		out.left -= count
	}
	out.truncated = out.truncated || count != len(data)
	return len(data), nil
}
