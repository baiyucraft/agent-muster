-- 历史答复的来源未知。新答复和来源在同一事务中写入。
ALTER TABLE muster_approvals
    ADD COLUMN IF NOT EXISTS answered_app_id text,
    ADD COLUMN IF NOT EXISTS answered_open_id text,
    ADD COLUMN IF NOT EXISTS answered_via text CHECK (CASE
        WHEN answered_via IS NULL THEN answered_app_id IS NULL AND answered_open_id IS NULL
        WHEN answered_via = 'web' THEN answered_at IS NOT NULL AND answered_app_id IS NULL AND answered_open_id IS NULL
        WHEN answered_via = 'feishu' THEN answered_at IS NOT NULL
            AND COALESCE(answered_app_id, '') <> '' AND COALESCE(answered_open_id, '') <> ''
        ELSE false
    END);

-- 旧卡片默认未显示等待设备。NULL 表示某次更新结果未确认。
ALTER TABLE muster_approval_notifications
    ADD COLUMN IF NOT EXISTS sent_waiting_device boolean DEFAULT false,
    ADD COLUMN IF NOT EXISTS leased_waiting_device boolean NOT NULL DEFAULT false;
