-- 设备注册有独立租期，表示该设备近期可以参与 WebRTC 信令交换。
CREATE TABLE IF NOT EXISTS muster_rtc_devices (
    device_id text PRIMARY KEY REFERENCES muster_devices(device_id),
    expires_at timestamptz NOT NULL
);

-- 每次投递只保留当前连接尝试的信令。实际投递内容通过设备数据通道传输。
CREATE TABLE IF NOT EXISTS muster_rtc_signals (
    transfer_id text PRIMARY KEY REFERENCES muster_transfers(id),
    attempt_id text NOT NULL,
    offer text NOT NULL,
    answer text NOT NULL DEFAULT '',
    expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS muster_rtc_signals_expiry ON muster_rtc_signals(expires_at);
CREATE INDEX IF NOT EXISTS muster_transfers_pending_target ON muster_transfers(target_device_id, created_at)
    WHERE received_at IS NULL;
