//go:build !windows

package work

import (
	"context"
	"io/fs"
	"time"

	"github.com/baiyucraft/agent-muster/internal/runreport"
)

const runLogPlatformSupported = false

func readLocalRunLogs(string, string) (runreport.LogResponse, error) {
	return runreport.LogResponse{}, fs.ErrNotExist
}

func (*Worker) pruneRunLogs(context.Context, time.Time) error { return nil }
