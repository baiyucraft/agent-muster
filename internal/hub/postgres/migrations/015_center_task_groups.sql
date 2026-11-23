ALTER TABLE muster_tasks
    ADD COLUMN IF NOT EXISTS center_group boolean NOT NULL DEFAULT false,
    ALTER COLUMN project_id DROP NOT NULL,
    ALTER COLUMN device_id DROP NOT NULL,
    ALTER COLUMN directory_id DROP NOT NULL,
    ALTER COLUMN current_run_id DROP NOT NULL,
    ALTER COLUMN binding_id DROP NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'muster_task_group_route' AND conrelid = 'muster_tasks'::regclass) THEN
        ALTER TABLE muster_tasks ADD CONSTRAINT muster_task_group_route CHECK (
            (center_group AND parent_id IS NULL AND root_id = id AND project_id IS NULL AND device_id IS NULL
                AND directory_id IS NULL AND current_run_id IS NULL AND binding_id IS NULL)
            OR (NOT center_group AND project_id IS NOT NULL AND device_id IS NOT NULL
                AND directory_id IS NOT NULL AND current_run_id IS NOT NULL AND binding_id IS NOT NULL)
        );
    END IF;
END $$;

ALTER TABLE muster_deliveries ADD COLUMN IF NOT EXISTS plan_digest bytea;

CREATE TABLE IF NOT EXISTS muster_center_task_dependencies (
    task_id text NOT NULL REFERENCES muster_tasks(id),
    prerequisite_id text NOT NULL REFERENCES muster_tasks(id),
    PRIMARY KEY (task_id, prerequisite_id),
    CHECK (task_id <> prerequisite_id)
);
