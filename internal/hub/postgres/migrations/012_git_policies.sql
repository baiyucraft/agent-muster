ALTER TABLE muster_projects
    ADD COLUMN IF NOT EXISTS git_policy jsonb,
    ADD COLUMN IF NOT EXISTS git_policy_revision bigint NOT NULL DEFAULT 0;
