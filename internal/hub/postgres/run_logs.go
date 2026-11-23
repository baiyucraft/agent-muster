package postgres

import (
	"context"

	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/baiyucraft/agent-muster/internal/hub/workspace"
)

// RunLogDevice 查找已结束的运行的实际执行设备，并检查登记及心跳。
// taskID 和 runID 必须属于同一记录，已撤销或离线设备不接受读取请求。
func (store *InventoryStore) RunLogDevice(ctx context.Context, taskID, runID string) (string, error) {
	var deviceID string
	var finished, revoked, online bool
	err := store.pool.QueryRow(ctx, `
		SELECT r.device_id, r.claimed_at IS NOT NULL AND r.status NOT IN ('queued','running'),
			d.revoked_at IS NOT NULL,
			COALESCE(d.last_seen_at > statement_timestamp() - ($3::double precision * interval '1 second'), false)
		FROM muster_runs r JOIN muster_devices d ON d.device_id=r.device_id
		WHERE r.task_id=$1 AND r.id=$2`, taskID, runID, registry.OnlineWindow.Seconds()).
		Scan(&deviceID, &finished, &revoked, &online)
	switch {
	case err != nil:
		return "", notFound(err)
	case !finished:
		return "", workspace.ErrRunLogsUnavailable
	case revoked:
		return "", workspace.ErrRunLogDeviceRevoked
	case !online:
		return "", workspace.ErrRunLogDeviceOffline
	default:
		return deviceID, nil
	}
}

// AuthenticateRunLogDevice 核对日志轮询和回执使用未撤销的执行设备凭据。
func (store *InventoryStore) AuthenticateRunLogDevice(ctx context.Context, deviceID string, token [32]byte) error {
	var authorized bool
	err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM muster_devices
		WHERE device_id=$1 AND token_hash=$2 AND revoked_at IS NULL)`, deviceID, token[:]).Scan(&authorized)
	if err == nil && !authorized {
		return registry.ErrUnauthorized
	}
	return err
}
