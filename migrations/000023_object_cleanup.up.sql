CREATE TABLE object_delete_jobs (
    object_key TEXT PRIMARY KEY,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_object_delete_jobs_due ON object_delete_jobs(available_at, object_key);

-- Recover historical soft deletes whose object removal may have failed.
INSERT INTO object_delete_jobs(object_key)
SELECT object_key FROM videos WHERE status = 'DELETED'
UNION SELECT cover_key FROM videos WHERE status = 'DELETED' AND cover_key IS NOT NULL
ON CONFLICT DO NOTHING;

-- Old PUT URLs target final objects; require new staging tickets on upgrade.
UPDATE upload_tickets SET expires_at = least(expires_at, now()) WHERE used = false;
