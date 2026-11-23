//go:build !windows

package work

import (
	"io/fs"

	"github.com/baiyucraft/agent-muster/internal/runreport"
)

const runLogPlatformSupported = false

func readLocalRunLogs(string, string) (runreport.LogResponse, error) {
	return runreport.LogResponse{}, fs.ErrNotExist
}
