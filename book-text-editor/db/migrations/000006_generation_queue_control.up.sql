BEGIN;

ALTER TABLE jobs
    DROP CONSTRAINT jobs_status_check;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_status_check CHECK (
        status IN (
            'queued',
            'running',
            'paused',
            'canceled',
            'completed',
            'completed_with_warnings',
            'failed'
        )
    );

CREATE INDEX jobs_generation_queue_idx
    ON jobs (created_at ASC, id ASC)
    WHERE status IN ('queued', 'running', 'paused');

COMMIT;
