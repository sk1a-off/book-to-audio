BEGIN;

DROP INDEX IF EXISTS jobs_generation_queue_idx;

UPDATE jobs
SET status = CASE
    WHEN status = 'paused' THEN 'queued'
    WHEN status = 'canceled' THEN 'failed'
    ELSE status
END
WHERE status IN ('paused', 'canceled');

ALTER TABLE jobs
    DROP CONSTRAINT jobs_status_check;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_status_check CHECK (
        status IN (
            'queued',
            'running',
            'completed',
            'completed_with_warnings',
            'failed'
        )
    );

COMMIT;
