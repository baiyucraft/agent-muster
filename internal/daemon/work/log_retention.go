package work

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

const logRetentionEnv = "MUSTER_RUN_LOG_RETENTION_DAYS"

func configuredLogRetention() time.Duration {
	retention, err := parseLogRetention(os.Getenv(logRetentionEnv))
	if err != nil {
		slog.Warn("已禁用设备日志自动清理", "setting", logRetentionEnv, "error", err)
	}
	return retention
}

func parseLogRetention(value string) (time.Duration, error) {
	const day = 24 * time.Hour
	value = strings.TrimSpace(value)
	if value == "" {
		return 30 * day, nil
	}
	days, err := strconv.ParseUint(value, 10, 64)
	if err != nil || days > uint64((1<<63-1)/day) {
		return 0, errors.New("日志留存天数必须为非负整数且在支持范围内")
	}
	return time.Duration(days) * day, nil
}

// confirmedRunForLogRetention 仅接受已终止且不再等待 Hub 确认的运行。
// 新旧 SQLite 都在更新状态和序号时写入 outbox，收到 Hub 确认后才移除回执。
func (worker *Worker) confirmedRunForLogRetention(ctx context.Context, runID string) (bool, error) {
	var confirmed bool
	err := worker.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM runs r WHERE r.id=? AND r.sequence > 0
			AND r.state IN ('completed','failed','interrupted')
			AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.run_id=r.id)
			AND NOT EXISTS (SELECT 1 FROM git_push_attempts p WHERE p.run_id=r.id)
	)`, runID).Scan(&confirmed)
	return confirmed, err
}

func (worker *Worker) runLogRetentionLoop(ctx context.Context, retention time.Duration) {
	for ctx.Err() == nil {
		if err := worker.pruneRunLogs(ctx, time.Now().Add(-retention)); err != nil && ctx.Err() == nil {
			slog.Warn("清理设备运行日志失败", "error", err)
		}
		if !sleepContext(ctx, time.Hour) {
			return
		}
	}
}
