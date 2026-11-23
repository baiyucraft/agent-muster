ALTER TABLE muster_runs
    ADD COLUMN IF NOT EXISTS session_retry_after timestamptz,
    ADD COLUMN IF NOT EXISTS session_retry_origin text REFERENCES muster_runs(id);

ALTER TABLE muster_run_inputs
    ADD COLUMN IF NOT EXISTS previous_delivery_run_id text REFERENCES muster_runs(id);
