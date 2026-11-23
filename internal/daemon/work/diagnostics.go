package work

import (
	"context"
	"path/filepath"
	"regexp"

	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

var logRunID = regexp.MustCompile(`^run-[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
var taskStorageID = regexp.MustCompile(`^task-[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func (worker *Worker) captureRun(ctx context.Context, id string) (context.Context, func() *runreport.Diagnostics) {
	directory := worker.runLogDirectory(id)
	if directory == "" {
		return ctx, func() *runreport.Diagnostics { return nil }
	}
	return runner.WithDiagnostics(ctx, directory)
}

func (worker *Worker) runLogDirectory(id string) string {
	if worker.root == "" || !logRunID.MatchString(id) {
		return ""
	}
	return filepath.Join(worker.root, "logs", id)
}
