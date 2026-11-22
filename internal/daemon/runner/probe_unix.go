//go:build !windows

package runner

import (
	"context"
	"os/exec"
)

func probeCommand(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, path, args...), nil
}
