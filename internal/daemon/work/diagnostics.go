package work

import (
	"context"
	"path/filepath"
	"regexp"

	"github.com/baiyucraft/agent-muster/internal/daemon/runner"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

var logRunID = regexp.MustCompile(`^run-[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func (worker *Worker) captureRun(ctx context.Context, id string) (context.Context, func() *runreport.Diagnostics) {
	if worker.root == "" || !logRunID.MatchString(id) {
		return ctx, func() *runreport.Diagnostics { return nil }
	}
	return runner.WithDiagnostics(ctx, filepath.Join(worker.root, "logs", id))
}
