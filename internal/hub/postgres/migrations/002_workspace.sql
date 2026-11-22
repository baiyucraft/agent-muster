CREATE TABLE IF NOT EXISTS muster_workspace (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    revision bigint NOT NULL DEFAULT 0
);
INSERT INTO muster_workspace (singleton) VALUES (true) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS muster_workspace_events (
    revision bigint PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS muster_directories (
    device_id text NOT NULL REFERENCES muster_devices,
    directory_id text NOT NULL,
    label text NOT NULL,
    PRIMARY KEY (device_id, directory_id)
);

CREATE TABLE IF NOT EXISTS muster_projects (
    id text PRIMARY KEY,
    name text NOT NULL,
    device_id text NOT NULL REFERENCES muster_devices,
    directory_id text NOT NULL,
    directory_label text NOT NULL
);

CREATE TABLE IF NOT EXISTS muster_tasks (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES muster_projects,
    project_name text NOT NULL,
    device_id text NOT NULL REFERENCES muster_devices,
    directory_id text NOT NULL,
    title text NOT NULL,
    status text NOT NULL,
    result text NOT NULL DEFAULT '',
    session_id text NOT NULL DEFAULT '',
    current_run_id text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS muster_messages (
    id text PRIMARY KEY,
    sequence bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    kind text NOT NULL,
    body text NOT NULL,
    requested_task_id text NOT NULL DEFAULT '',
    task_id text REFERENCES muster_tasks,
    status text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS muster_runs (
    id text PRIMARY KEY,
    sequence bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    task_id text NOT NULL REFERENCES muster_tasks,
    input text NOT NULL,
    status text NOT NULL DEFAULT 'queued',
    claimed_at timestamptz,
    input_session text NOT NULL DEFAULT '',
    session_id text NOT NULL DEFAULT '',
    outcome text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS muster_runs_task_order ON muster_runs (task_id, sequence);

CREATE TABLE IF NOT EXISTS muster_run_events (
    run_id text NOT NULL REFERENCES muster_runs,
    sequence bigint NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (run_id, sequence)
);

CREATE TABLE IF NOT EXISTS muster_deliveries (
    id text PRIMARY KEY,
    sequence bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    kind text NOT NULL,
    body text NOT NULL,
    message_id text REFERENCES muster_messages,
    task_id text REFERENCES muster_tasks,
    dispatch_task_id text REFERENCES muster_tasks,
    lease text NOT NULL DEFAULT '',
    lease_until timestamptz,
    attempt integer NOT NULL DEFAULT 1,
    available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    done boolean NOT NULL DEFAULT false,
    completion jsonb
);
