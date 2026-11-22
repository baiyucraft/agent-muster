-- 验收公钥只记录首次声明，发行时仍需核对设备与公钥的绑定关系。
CREATE TABLE IF NOT EXISTS muster_rtc_public_keys (
    device_id text PRIMARY KEY REFERENCES muster_devices(device_id) ON DELETE CASCADE,
    public_key bytea NOT NULL CHECK (octet_length(public_key) = 32),
    registered_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
