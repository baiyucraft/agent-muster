ALTER TABLE muster_runs
    ADD COLUMN IF NOT EXISTS input_receipt_supported boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS input_receipts_frozen boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS muster_task_inputs (
    task_id text NOT NULL REFERENCES muster_tasks(id),
    id text NOT NULL,
    origin_run_id text NOT NULL REFERENCES muster_runs(id),
    message_id text REFERENCES muster_messages(id),
    transfer_id text REFERENCES muster_transfers(id),
    delivered_run_id text REFERENCES muster_runs(id),
    handled_run_id text REFERENCES muster_runs(id),
    handled_at timestamptz,
    PRIMARY KEY (task_id, id),
    CHECK ((message_id IS NOT NULL AND transfer_id IS NULL) OR (message_id IS NULL AND transfer_id IS NOT NULL)),
    CHECK ((handled_run_id IS NULL) = (handled_at IS NULL))
);

CREATE TABLE IF NOT EXISTS muster_run_inputs (
    run_id text NOT NULL REFERENCES muster_runs(id),
    task_id text NOT NULL,
    input_id text NOT NULL,
    PRIMARY KEY (run_id, input_id),
    FOREIGN KEY (task_id, input_id) REFERENCES muster_task_inputs(task_id, id)
);

-- 只补齐尚未开始的历史输入，已领取轮次没有处理回执可以验证。
INSERT INTO muster_task_inputs (task_id,id,origin_run_id,message_id)
SELECT r.task_id,'message:' || m.id,r.id,m.id
FROM muster_messages m JOIN muster_runs r ON r.id='run-' || m.id AND r.task_id=m.task_id
WHERE m.kind='user' AND r.claimed_at IS NULL AND r.status='queued' AND r.id <> 'run-' || substr(r.task_id,6)
ON CONFLICT DO NOTHING;

INSERT INTO muster_task_inputs (task_id,id,origin_run_id,transfer_id)
SELECT DISTINCT ON (r.task_id,tr.id) r.task_id,'transfer:' || tr.id,r.id,tr.id
FROM muster_run_transfers rt JOIN muster_runs r ON r.id=rt.run_id
JOIN muster_transfers tr ON tr.id=rt.transfer_id
WHERE rt.direction='input' AND tr.kind<>'prompt' AND tr.id<>r.prompt_transfer_id
    AND r.claimed_at IS NULL AND r.status='queued'
ORDER BY r.task_id,tr.id,r.sequence
ON CONFLICT DO NOTHING;
