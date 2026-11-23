ALTER TABLE muster_tasks ADD COLUMN IF NOT EXISTS git_publish boolean NOT NULL DEFAULT false;
ALTER TABLE muster_runs ADD COLUMN IF NOT EXISTS git_review_supported boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS muster_git_push_intents (
    id text PRIMARY KEY,
    run_id text NOT NULL UNIQUE REFERENCES muster_runs(id),
    task_id text NOT NULL REFERENCES muster_tasks(id),
    policy_revision bigint NOT NULL,
    remote text NOT NULL,
    target_ref text NOT NULL,
    destination_fingerprint text NOT NULL,
    verification jsonb NOT NULL,
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','same_commit','contains_commit','missing','diverged','unavailable')),
    observed_oid text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS muster_git_push_intents_task ON muster_git_push_intents(task_id);
