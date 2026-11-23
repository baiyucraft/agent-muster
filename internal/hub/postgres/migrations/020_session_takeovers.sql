CREATE TABLE IF NOT EXISTS muster_session_takeovers (
    source_task_id text PRIMARY KEY REFERENCES muster_tasks(id),
    request_id uuid NOT NULL,
    summary_digest bytea NOT NULL CHECK (octet_length(summary_digest) = 32),
    task_id text NOT NULL UNIQUE REFERENCES muster_tasks(id),
    CHECK (source_task_id <> task_id)
);
