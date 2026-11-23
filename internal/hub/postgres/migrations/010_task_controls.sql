ALTER TABLE muster_tasks ADD COLUMN IF NOT EXISTS control_mode text NOT NULL DEFAULT ''
    CHECK (control_mode IN ('', 'pause', 'stop', 'cancel'));
ALTER TABLE muster_runs ADD COLUMN IF NOT EXISTS control_supported boolean NOT NULL DEFAULT false;
ALTER TABLE muster_approvals ADD COLUMN IF NOT EXISTS cancelled_at timestamptz;

CREATE TABLE IF NOT EXISTS muster_task_control_requests (
    task_id text NOT NULL REFERENCES muster_tasks(id),
    request_id uuid NOT NULL,
    action text NOT NULL CHECK (action IN ('pause','stop_current','resume','cancel')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (task_id, request_id)
);

ALTER TABLE muster_approval_notifications DROP CONSTRAINT IF EXISTS muster_approval_notifications_desired_status_check;
ALTER TABLE muster_approval_notifications DROP CONSTRAINT IF EXISTS muster_approval_notifications_sent_status_check;
ALTER TABLE muster_approval_notifications DROP CONSTRAINT IF EXISTS muster_approval_notifications_leased_status_check;
ALTER TABLE muster_approval_notifications ADD CHECK (desired_status IN ('pending','answered','handled','cancelled'));
ALTER TABLE muster_approval_notifications ADD CHECK (sent_status IN ('pending','answered','handled','cancelled'));
ALTER TABLE muster_approval_notifications ADD CHECK (leased_status IN ('pending','answered','handled','cancelled'));
