BEGIN;

ALTER TABLE fragment_text_revisions
    ADD COLUMN model_revision TEXT;

ALTER TABLE job_fragments
    ADD CONSTRAINT job_fragments_id_job_unique UNIQUE (id, job_id);

CREATE TABLE rewrite_tasks (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    model_id TEXT NOT NULL CHECK (btrim(model_id) <> ''),
    model_revision TEXT NOT NULL DEFAULT '',
    prompt TEXT NOT NULL CHECK (btrim(prompt) <> ''),
    temperature DOUBLE PRECISION NOT NULL CHECK (
        temperature >= 0 AND temperature <= 1
    ),
    top_k INTEGER NOT NULL CHECK (top_k BETWEEN 0 AND 200),
    top_p DOUBLE PRECISION NOT NULL CHECK (top_p BETWEEN 0 AND 1),
    min_p DOUBLE PRECISION NOT NULL CHECK (min_p BETWEEN 0 AND 1),
    repeat_penalty DOUBLE PRECISION NOT NULL CHECK (
        repeat_penalty BETWEEN 0.5 AND 2
    ),
    max_tokens INTEGER NOT NULL CHECK (max_tokens BETWEEN 16 AND 2048),
    status TEXT NOT NULL CHECK (
        status IN (
            'queued',
            'running',
            'completed',
            'completed_with_errors',
            'failed'
        )
    ),
    fragments_count INTEGER NOT NULL CHECK (fragments_count > 0),
    fragments_pending INTEGER NOT NULL CHECK (fragments_pending >= 0),
    fragments_completed INTEGER NOT NULL CHECK (fragments_completed >= 0),
    fragments_failed INTEGER NOT NULL CHECK (fragments_failed >= 0),
    error_message TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (id, job_id),
    CHECK (
        fragments_count =
        fragments_pending +
        fragments_completed +
        fragments_failed
    )
);

CREATE UNIQUE INDEX rewrite_tasks_one_active_per_job_idx
    ON rewrite_tasks (job_id)
    WHERE status IN ('queued', 'running');

CREATE INDEX rewrite_tasks_job_created_idx
    ON rewrite_tasks (job_id, created_at DESC, id DESC);

CREATE TABLE rewrite_task_fragments (
    rewrite_id TEXT NOT NULL,
    job_id TEXT NOT NULL,
    fragment_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    status TEXT NOT NULL CHECK (
        status IN ('queued', 'running', 'completed', 'failed')
    ),
    revision_id TEXT,
    changed BOOLEAN NOT NULL DEFAULT FALSE,
    reason TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    model_revision TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (rewrite_id, fragment_id),
    UNIQUE (rewrite_id, ordinal),
    FOREIGN KEY (rewrite_id, job_id)
        REFERENCES rewrite_tasks(id, job_id)
        ON DELETE CASCADE,
    FOREIGN KEY (fragment_id, job_id)
        REFERENCES job_fragments(id, job_id)
        ON DELETE CASCADE,
    FOREIGN KEY (revision_id, fragment_id)
        REFERENCES fragment_text_revisions(id, fragment_id)
        ON DELETE RESTRICT,
    CHECK (
        (changed AND revision_id IS NOT NULL)
        OR
        (NOT changed AND revision_id IS NULL)
    )
);

CREATE INDEX rewrite_task_fragments_status_idx
    ON rewrite_task_fragments (rewrite_id, status, ordinal);

COMMIT;
