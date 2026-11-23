ALTER TABLE muster_projects
    ADD COLUMN IF NOT EXISTS agent_kind text NOT NULL DEFAULT 'dsh';
CREATE TABLE IF NOT EXISTS muster_project_bindings (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES muster_projects(id),
    device_id text NOT NULL REFERENCES muster_devices(device_id),
    directory_id text NOT NULL,
    agent_kind text NOT NULL CHECK (agent_kind IN ('dsh', 'codex')),
    is_default boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX IF NOT EXISTS muster_project_default_binding
    ON muster_project_bindings(project_id) WHERE is_default;
INSERT INTO muster_project_bindings (id, project_id, device_id, directory_id, agent_kind, is_default)
SELECT p.id, p.id, p.device_id, p.directory_id, p.agent_kind, true
FROM muster_projects p WHERE NOT EXISTS (
    SELECT 1 FROM muster_project_bindings b WHERE b.project_id=p.id
);

ALTER TABLE muster_tasks
    ADD COLUMN IF NOT EXISTS binding_id text,
    ADD COLUMN IF NOT EXISTS waiting_reason text NOT NULL DEFAULT '';
UPDATE muster_tasks t SET binding_id = t.project_id WHERE t.binding_id IS NULL AND t.project_id IS NOT NULL;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'muster_tasks'::regclass AND attname = 'center_group' AND NOT attisdropped) THEN
        ALTER TABLE muster_tasks ALTER COLUMN binding_id SET NOT NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
        WHERE conname = 'muster_tasks_binding_fk' AND conrelid = 'muster_tasks'::regclass) THEN
        ALTER TABLE muster_tasks ADD CONSTRAINT muster_tasks_binding_fk
            FOREIGN KEY (binding_id) REFERENCES muster_project_bindings(id);
    END IF;
END $$;

ALTER TABLE muster_runs
    ADD COLUMN IF NOT EXISTS prompt_transfer_id text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS dependency_results jsonb,
    ADD COLUMN IF NOT EXISTS ready_priority smallint NOT NULL DEFAULT 3,
    ADD COLUMN IF NOT EXISTS legacy_events boolean NOT NULL DEFAULT false;
UPDATE muster_runs SET legacy_events=true WHERE claimed_at IS NOT NULL AND lease='';

CREATE TABLE IF NOT EXISTS muster_device_peers (
    device_id text PRIMARY KEY REFERENCES muster_devices(device_id),
    address text NOT NULL,
    fingerprint text NOT NULL
);
CREATE TABLE IF NOT EXISTS muster_transfers (
    id text PRIMARY KEY,
    run_id text NOT NULL REFERENCES muster_runs(id),
    task_id text NOT NULL REFERENCES muster_tasks(id),
    source_device_id text NOT NULL REFERENCES muster_devices(device_id),
    target_device_id text NOT NULL REFERENCES muster_devices(device_id),
    name text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('prompt', 'file', 'message', 'result')),
    sha256 text NOT NULL,
    size bigint NOT NULL CHECK (size BETWEEN 0 AND 33554432),
    ticket_hash bytea,
    ticket_expires_at timestamptz,
    received_at timestamptz,
    attempts integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS muster_transfers_source ON muster_transfers(source_device_id, created_at)
    WHERE received_at IS NULL;
CREATE INDEX IF NOT EXISTS muster_transfers_task ON muster_transfers(task_id, created_at);
ALTER TABLE muster_transfers ADD COLUMN IF NOT EXISTS last_error text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS muster_run_transfers (
    run_id text NOT NULL REFERENCES muster_runs(id),
    transfer_id text NOT NULL REFERENCES muster_transfers(id),
    direction text NOT NULL CHECK (direction IN ('input', 'result')),
    PRIMARY KEY (run_id, transfer_id, direction)
);
CREATE INDEX IF NOT EXISTS muster_run_transfers_lookup ON muster_run_transfers(transfer_id);
CREATE TABLE IF NOT EXISTS muster_wait_transfers (
    task_id text NOT NULL REFERENCES muster_tasks(id),
    generation bigint NOT NULL,
    transfer_id text NOT NULL REFERENCES muster_transfers(id),
    PRIMARY KEY (task_id,generation,transfer_id)
);
CREATE INDEX IF NOT EXISTS muster_wait_transfers_lookup ON muster_wait_transfers(transfer_id);
CREATE TABLE IF NOT EXISTS muster_delegations (
    parent_run_id text NOT NULL REFERENCES muster_runs(id),
    request_key text NOT NULL,
    child_task_id text NOT NULL REFERENCES muster_tasks(id),
    request_digest bytea NOT NULL,
    PRIMARY KEY (parent_run_id, request_key)
);
