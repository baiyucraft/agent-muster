ALTER TABLE muster_tasks ADD COLUMN IF NOT EXISTS git_policy jsonb;
ALTER TABLE muster_runs ADD COLUMN IF NOT EXISTS git_verification jsonb;
