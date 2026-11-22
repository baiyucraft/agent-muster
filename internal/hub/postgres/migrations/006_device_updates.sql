-- 每台设备独立保存上报版本和最近一次更新目标，设备离线时也可下发新目标。
CREATE TABLE IF NOT EXISTS muster_device_updates (
    device_id text PRIMARY KEY REFERENCES muster_devices(device_id) ON DELETE CASCADE,
    current_version text NOT NULL DEFAULT '',
    assignment_id text UNIQUE,
    target_version text,
    manifest_url text,
    assigned_at timestamptz,
    status text NOT NULL DEFAULT 'idle',
    last_error text NOT NULL DEFAULT '',
    reported_at timestamptz,
    CHECK (status IN ('idle', 'pending', 'downloading', 'waiting', 'applying', 'succeeded', 'failed', 'rolled_back')),
    -- 任务标识与版本、清单地址和分配时间必须同时存在。
    CHECK (
        (status = 'idle' AND assignment_id IS NULL AND target_version IS NULL AND manifest_url IS NULL AND assigned_at IS NULL)
        OR
        (status <> 'idle' AND assignment_id IS NOT NULL AND target_version IS NOT NULL AND manifest_url IS NOT NULL AND assigned_at IS NOT NULL)
    )
);
