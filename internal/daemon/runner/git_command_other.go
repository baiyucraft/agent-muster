//go:build !windows

package runner

import (
	"context"
	"errors"
	"io"
)

func runProjectCommand(context.Context, string, string, []string, io.Writer, io.Writer) (uint32, error) {
	return 0, errors.New("项目命令执行目前支持 Windows")
}
