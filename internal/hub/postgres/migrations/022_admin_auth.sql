-- 首个管理员由服务器维护命令写入。历史库迁移时不会创建默认账号。
CREATE TABLE IF NOT EXISTS muster_admin_credentials (
    singleton boolean PRIMARY KEY CHECK (singleton),
    username text NOT NULL CHECK (username <> ''),
    password_hash text NOT NULL CHECK (password_hash <> '')
);

CREATE TABLE IF NOT EXISTS muster_admin_sessions (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS muster_admin_sessions_expires_at_idx ON muster_admin_sessions (expires_at);
