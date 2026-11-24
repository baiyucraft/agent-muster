//go:build !windows

package runner

import (
	"context"
	"errors"
	"io"
	"os/exec"
)

func runInJob(context.Context, *exec.Cmd, string, func() error, func(io.Reader) error, io.Writer) (uint32, error) {
	return 0, errors.New("任务执行目前支持 Windows")
}
