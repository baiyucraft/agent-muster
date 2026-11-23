CREATE TABLE IF NOT EXISTS muster_approvals (
    id text PRIMARY KEY REFERENCES muster_messages(id),
    task_id text NOT NULL REFERENCES muster_tasks(id),
    run_id text NOT NULL UNIQUE REFERENCES muster_runs(id),
    question text NOT NULL,
    options jsonb NOT NULL,
    impact text NOT NULL DEFAULT '',
    answer text NOT NULL DEFAULT '',
    resume_run_id text UNIQUE REFERENCES muster_runs(id),
    created_at timestamptz NOT NULL,
    answered_at timestamptz,
    handled_at timestamptz,
    CHECK (
        (answered_at IS NULL AND answer = '' AND resume_run_id IS NULL AND handled_at IS NULL)
        OR (answered_at IS NOT NULL AND answer <> '' AND resume_run_id IS NOT NULL)
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS muster_approvals_pending_task ON muster_approvals(task_id)
    WHERE answered_at IS NULL;
CREATE INDEX IF NOT EXISTS muster_approvals_task ON muster_approvals(task_id, created_at, id);
