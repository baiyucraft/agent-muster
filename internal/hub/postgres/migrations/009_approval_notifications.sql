CREATE TABLE IF NOT EXISTS muster_approval_notifications (
    approval_id text PRIMARY KEY REFERENCES muster_approvals(id),
    app_id text NOT NULL,
    open_id text NOT NULL,
    message_id text,
    desired_status text NOT NULL CHECK (desired_status IN ('pending','answered','handled')),
    sent_status text CHECK (sent_status IN ('pending','answered','handled')),
    first_send_at timestamptz,
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    attempts integer NOT NULL DEFAULT 0,
    lease text,
    lease_until timestamptz,
    leased_status text CHECK (leased_status IN ('pending','answered','handled')),
    UNIQUE (app_id, message_id),
    CHECK ((message_id IS NULL) = (sent_status IS NULL)),
    CHECK ((lease IS NULL) = (lease_until IS NULL) AND (lease IS NULL) = (leased_status IS NULL))
);
CREATE INDEX IF NOT EXISTS muster_approval_notifications_due ON muster_approval_notifications(app_id, open_id, next_attempt_at)
    WHERE sent_status IS DISTINCT FROM desired_status;
