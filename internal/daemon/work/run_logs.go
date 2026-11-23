package work

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"time"

	"github.com/baiyucraft/agent-muster/internal/daemon/control"
	"github.com/baiyucraft/agent-muster/internal/runreport"
)

type runLogClient interface {
	RunLogRequests(context.Context, control.Identity) ([]runreport.LogRequest, error)
	ReportRunLogs(context.Context, control.Identity, runreport.LogRequest, runreport.LogResponse) error
}

func (worker *Worker) runLogLoop(ctx context.Context, client runLogClient, identity control.Identity) {
	const pollInterval = 2 * time.Second
	for ctx.Err() == nil {
		err := worker.respondToLogRequests(ctx, client, identity)
		var status *control.HTTPError
		if errors.Is(err, control.ErrUnauthorized) {
			return
		}
		if errors.As(err, &status) && status.Status == http.StatusNotFound {
			// 旧版 Hub 没有此接口，稍后复查以支持 Hub 在线升级。
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}
		if !sleepContext(ctx, pollInterval) {
			return
		}
	}
}

func (worker *Worker) respondToLogRequests(ctx context.Context, client runLogClient, identity control.Identity) error {
	requests, err := client.RunLogRequests(ctx, identity)
	if err != nil {
		return err
	}
	for _, request := range requests {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logs := worker.loadRunLogs(ctx, request)
		err := client.ReportRunLogs(ctx, identity, request, logs)
		var status *control.HTTPError
		if errors.As(err, &status) && status.Status == http.StatusNotFound {
			continue // 浏览器取消或请求过期时，忽略该次响应。
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (worker *Worker) loadRunLogs(ctx context.Context, request runreport.LogRequest) runreport.LogResponse {
	if request.Validate() != nil {
		return runreport.LogResponse{Status: runreport.LogFailed}
	}
	var known int
	err := worker.db.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id=? AND state IN ('completed','failed','interrupted')`, request.RunID).Scan(&known)
	if errors.Is(err, sql.ErrNoRows) {
		return runreport.LogResponse{Status: runreport.LogMissing}
	}
	if err != nil {
		return runreport.LogResponse{Status: runreport.LogFailed}
	}
	worker.logMu.Lock()
	logs, err := readLocalRunLogs(worker.root, request.RunID, request.Invocation)
	worker.logMu.Unlock()
	if errors.Is(err, fs.ErrNotExist) {
		return runreport.LogResponse{Status: runreport.LogMissing}
	}
	if err != nil || logs.Validate() != nil {
		return runreport.LogResponse{Status: runreport.LogFailed}
	}
	return logs
}
