package postgres

import (
	"context"
	_ "embed"
	"errors"
	"time"

	"github.com/baiyucraft/agent-muster/internal/deviceupdate"
	"github.com/baiyucraft/agent-muster/internal/hub/registry"
	"github.com/jackc/pgx/v5"
)

//go:embed migrations/006_device_updates.sql
var createDeviceUpdates string

func migrateDeviceUpdates(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, createDeviceUpdates, pgx.QueryExecModeSimpleProtocol)
	return err
}

const deviceUpdateColumns = `
	d.device_id, COALESCE(u.current_version, ''), u.assignment_id, u.target_version, u.manifest_url,
	u.assigned_at, COALESCE(u.status, 'idle'), COALESCE(u.last_error, ''), u.reported_at
`

func scanDeviceUpdate(row pgx.Row) (state deviceupdate.DeviceState, err error) {
	var id, version, manifestURL *string
	var assignedAt *time.Time
	err = row.Scan(&state.DeviceID, &state.CurrentVersion, &id, &version, &manifestURL,
		&assignedAt, &state.Status, &state.Error, &state.ReportedAt)
	if id != nil && version != nil && manifestURL != nil && assignedAt != nil {
		state.Target = &deviceupdate.Target{
			ID: *id, Version: *version, ManifestURL: *manifestURL, AssignedAt: assignedAt.UTC(),
		}
	}
	return state, err
}

// ListDeviceUpdates 返回所有登记设备的更新状态，以及设备最后一次上报的版本。
// 尚未上报的设备也会出现在结果中。
func (store *InventoryStore) ListDeviceUpdates(ctx context.Context) ([]deviceupdate.DeviceState, error) {
	rows, err := store.pool.Query(ctx, `SELECT `+deviceUpdateColumns+` FROM muster_devices d
		LEFT JOIN muster_device_updates u USING (device_id)
		ORDER BY d.device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := []deviceupdate.DeviceState{}
	for rows.Next() {
		state, err := scanDeviceUpdate(rows)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

// AssignDeviceUpdate 为未撤销的设备保存更新目标，设备下次轮询时可领取。
// 重复下发相同目标返回已有任务。显式重试失败或回滚的任务时使用新的任务 ID。
func (store *InventoryStore) AssignDeviceUpdate(ctx context.Context, deviceID, assignmentID string, assignment deviceupdate.Assignment) (state deviceupdate.DeviceState, err error) {
	err = pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := lockUpdateDevice(ctx, tx, deviceID, nil); err != nil {
			return err
		}
		state, err = readDeviceUpdate(ctx, tx, deviceID)
		if err != nil {
			return err
		}
		sameTarget := state.Target != nil && state.Target.Version == assignment.Version &&
			state.Target.ManifestURL == assignment.ManifestURL
		canRetry := state.Status == deviceupdate.StatusFailed || state.Status == deviceupdate.StatusRolledBack
		if sameTarget && (!assignment.Retry || !canRetry) {
			return nil
		}
		if assignment.Retry && !sameTarget {
			return deviceupdate.ErrConflict
		}
		// applying 是设备开始切换前获得的确认，此后目标需保留到设备上报结果。
		if state.Status == deviceupdate.StatusApplying ||
			(state.CurrentVersion != "" && deviceupdate.CompareVersion(assignment.Version, state.CurrentVersion) <= 0) {
			return deviceupdate.ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO muster_device_updates
			(device_id, assignment_id, target_version, manifest_url, assigned_at, status)
			VALUES ($1, $2, $3, $4, clock_timestamp(), 'pending')
			ON CONFLICT (device_id) DO UPDATE SET
				assignment_id=excluded.assignment_id, target_version=excluded.target_version,
				manifest_url=excluded.manifest_url, assigned_at=excluded.assigned_at,
				status=excluded.status, last_error=''`, deviceID, assignmentID, assignment.Version, assignment.ManifestURL)
		if err != nil {
			return err
		}
		state, err = readDeviceUpdate(ctx, tx, deviceID)
		return err
	})
	return state, err
}

// CheckDeviceUpdate 在同一事务中认证设备、保存版本和进度，并返回待处理目标。
// 处于安装切换中的设备也能重新取回原目标，以恢复超时或中断后的安装。
func (store *InventoryStore) CheckDeviceUpdate(ctx context.Context, deviceID string, token [32]byte, check deviceupdate.CheckRequest) (target *deviceupdate.Target, err error) {
	err = pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		if err := lockUpdateDevice(ctx, tx, deviceID, &token); err != nil {
			return err
		}
		state, err := readDeviceUpdate(ctx, tx, deviceID)
		if err != nil {
			return err
		}
		state.CurrentVersion = check.Version
		advanceDeviceUpdate(&state, check.Report)
		_, err = tx.Exec(ctx, `INSERT INTO muster_device_updates (device_id, current_version, reported_at)
			VALUES ($1, $2, clock_timestamp()) ON CONFLICT (device_id) DO UPDATE SET
				current_version=excluded.current_version, reported_at=excluded.reported_at,
				status=$3, last_error=$4`, deviceID, check.Version, state.Status, state.Error)
		if err == nil && !deviceupdate.Terminal(state.Status) {
			target = state.Target
		}
		return err
	})
	return target, err
}

// lockUpdateDevice 锁定设备登记行，使撤销、分配及轮询按设备串行生效。
// 第一次轮询尚无更新记录时，此锁也能覆盖更新记录的创建。
func lockUpdateDevice(ctx context.Context, tx pgx.Tx, deviceID string, token *[32]byte) error {
	var id string
	if token == nil {
		err := tx.QueryRow(ctx, `SELECT device_id FROM muster_devices
			WHERE device_id=$1 AND revoked_at IS NULL FOR UPDATE`, deviceID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		return err
	}
	err := tx.QueryRow(ctx, `SELECT device_id FROM muster_devices
		WHERE device_id=$1 AND token_hash=$2 AND revoked_at IS NULL FOR UPDATE`, deviceID, token[:]).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return registry.ErrUnauthorized
	}
	return err
}

func readDeviceUpdate(ctx context.Context, tx pgx.Tx, deviceID string) (deviceupdate.DeviceState, error) {
	return scanDeviceUpdate(tx.QueryRow(ctx, `SELECT `+deviceUpdateColumns+` FROM muster_devices d
		LEFT JOIN muster_device_updates u USING (device_id)
		WHERE d.device_id=$1`, deviceID))
}

func advanceDeviceUpdate(state *deviceupdate.DeviceState, report *deviceupdate.Report) {
	if state.Target == nil {
		return
	}
	// 新进程首次报告目标版本时即可确认成功，即使安装回执在重启期间丢失。
	if state.CurrentVersion == state.Target.Version {
		state.Status, state.Error = deviceupdate.StatusSucceeded, ""
		return
	}
	if deviceupdate.Terminal(state.Status) {
		return
	}
	// 只处理当前任务的回执，避免旧进程或上一次重试的回执改变新目标的进度。
	if report != nil && report.AssignmentID == state.Target.ID &&
		acceptUpdateProgress(state.Status, report.Status, state.CurrentVersion, state.Target.Version) {
		state.Status, state.Error = report.Status, report.Error
		return
	}
	if state.Status == deviceupdate.StatusApplying {
		return
	}
	switch deviceupdate.CompareVersion(state.CurrentVersion, state.Target.Version) {
	case 0:
		state.Status = deviceupdate.StatusSucceeded
	case 1:
		state.Status, state.Error = deviceupdate.StatusFailed, "device already runs a newer version"
	}
}

// acceptUpdateProgress 允许进度重发，并按安装阶段和实际版本判断状态能否推进。
func acceptUpdateProgress(current, next, currentVersion, targetVersion string) bool {
	if current == next || next == deviceupdate.StatusFailed {
		return true
	}
	needsUpgrade := deviceupdate.CompareVersion(currentVersion, targetVersion) < 0
	switch next {
	case deviceupdate.StatusDownloading:
		return needsUpgrade && current == deviceupdate.StatusPending
	case deviceupdate.StatusWaiting:
		return needsUpgrade && (current == deviceupdate.StatusPending || current == deviceupdate.StatusDownloading)
	case deviceupdate.StatusApplying:
		return needsUpgrade
	case deviceupdate.StatusSucceeded:
		return current == deviceupdate.StatusApplying && currentVersion == targetVersion
	case deviceupdate.StatusRolledBack:
		return current == deviceupdate.StatusApplying && needsUpgrade
	default:
		return false
	}
}
