BEGIN;

DROP TABLE IF EXISTS rewrite_task_fragments;
DROP TABLE IF EXISTS rewrite_tasks;
ALTER TABLE job_fragments
    DROP CONSTRAINT IF EXISTS job_fragments_id_job_unique;
ALTER TABLE fragment_text_revisions
    DROP COLUMN IF EXISTS model_revision;

COMMIT;
