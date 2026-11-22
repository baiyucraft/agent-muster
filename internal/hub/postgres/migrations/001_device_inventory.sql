CREATE TABLE IF NOT EXISTS muster_pairing_codes (
    code_hash bytea PRIMARY KEY,
    device_name text NOT NULL,
    expires_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS muster_devices (
    device_id text PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE,
    name text NOT NULL,
    hostname text NOT NULL,
    os text NOT NULL,
    arch text NOT NULL,
    registered_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_seen_at timestamptz,
    probed_at timestamptz,
    revoked_at timestamptz,
    inventory jsonb NOT NULL DEFAULT '{"agents":[]}'
);
