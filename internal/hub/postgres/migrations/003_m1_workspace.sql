ALTER TABLE muster_tasks
    ADD COLUMN IF NOT EXISTS parent_id text REFERENCES muster_tasks(id),
    ADD COLUMN IF NOT EXISTS root_id text REFERENCES muster_tasks(id),
    ADD COLUMN IF NOT EXISTS input_generation bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS agent_kind text NOT NULL DEFAULT 'dsh';
UPDATE muster_tasks SET root_id = id WHERE root_id IS NULL;
ALTER TABLE muster_tasks ALTER COLUMN root_id SET NOT NULL;

CREATE INDEX IF NOT EXISTS muster_tasks_parent_order ON muster_tasks(parent_id, updated_at, id);
CREATE INDEX IF NOT EXISTS muster_tasks_root_order ON muster_tasks(root_id, updated_at, id);

ALTER TABLE muster_runs
    ADD COLUMN IF NOT EXISTS generation bigint NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS lease text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS lease_until timestamptz,
    ADD COLUMN IF NOT EXISTS device_id text REFERENCES muster_devices(device_id);
UPDATE muster_runs r SET device_id=t.device_id FROM muster_tasks t WHERE r.task_id=t.id AND r.device_id IS NULL;
ALTER TABLE muster_runs ALTER COLUMN device_id SET NOT NULL;
CREATE INDEX IF NOT EXISTS muster_runs_claim_order ON muster_runs(device_id, status, sequence);

CREATE TABLE IF NOT EXISTS muster_task_dependencies (
    task_id text NOT NULL REFERENCES muster_tasks(id),
    depends_on_task_id text NOT NULL REFERENCES muster_tasks(id),
    generation bigint NOT NULL,
    status text NOT NULL DEFAULT 'waiting',
    PRIMARY KEY (task_id, depends_on_task_id, generation),
    CHECK (task_id <> depends_on_task_id)
);
CREATE INDEX IF NOT EXISTS muster_dependencies_child ON muster_task_dependencies(depends_on_task_id);
