-- Application-owned account recovery history. No account or usage data changes.
CREATE TABLE IF NOT EXISTS account_recovery_runs (
    id BIGSERIAL PRIMARY KEY,
    trigger TEXT NOT NULL CHECK (trigger IN ('schedule', 'manual')),
    status TEXT NOT NULL CHECK (status IN ('running', 'success', 'partial', 'failed', 'interrupted')),
    scheduled_for TIMESTAMPTZ,
    request_id TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    config_revision TEXT NOT NULL,
    outcome JSONB NOT NULL DEFAULT '{}'::jsonb,
    error_message TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS account_recovery_runs_slot
    ON account_recovery_runs (scheduled_for) WHERE scheduled_for IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS account_recovery_runs_request
    ON account_recovery_runs (request_id) WHERE request_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS account_recovery_runs_recent ON account_recovery_runs (started_at DESC);
