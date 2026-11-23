CREATE TABLE IF NOT EXISTS muster_binding_proposals (
    id text PRIMARY KEY REFERENCES muster_messages(id),
    message_id text NOT NULL UNIQUE REFERENCES muster_messages(id),
    request_digest bytea NOT NULL,
    name text NOT NULL,
    bindings jsonb NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    project_id text REFERENCES muster_projects(id),
    created_at timestamptz NOT NULL,
    decided_at timestamptz,
    CHECK ((status = 'pending' AND project_id IS NULL AND decided_at IS NULL)
        OR (status = 'accepted' AND project_id IS NOT NULL AND decided_at IS NOT NULL)
        OR (status = 'rejected' AND project_id IS NULL AND decided_at IS NOT NULL))
);
