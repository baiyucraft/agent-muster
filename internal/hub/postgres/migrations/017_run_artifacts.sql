CREATE TABLE IF NOT EXISTS muster_run_artifacts (
    run_id text NOT NULL,
    id text NOT NULL CHECK (id ~ '^[0-9a-f]{64}$'),
    task_id text NOT NULL REFERENCES muster_tasks(id),
    device_id text NOT NULL REFERENCES muster_devices(device_id),
    event_sequence bigint NOT NULL,
    relative_path text NOT NULL,
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size bigint NOT NULL CHECK (size >= 0),
    audit_only boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (run_id,id),
    FOREIGN KEY (run_id,event_sequence) REFERENCES muster_run_events(run_id,sequence)
);

CREATE INDEX IF NOT EXISTS muster_run_artifacts_task_order
    ON muster_run_artifacts(task_id,created_at DESC,id);
