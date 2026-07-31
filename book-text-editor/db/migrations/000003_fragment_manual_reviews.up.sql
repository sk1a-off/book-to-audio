BEGIN;

CREATE TABLE fragment_manual_reviews (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    fragment_id TEXT NOT NULL
        REFERENCES job_fragments(id)
        ON DELETE CASCADE,
    attempt INTEGER NOT NULL CHECK (attempt >= 0),
    decision TEXT NOT NULL CHECK (decision = 'approved'),
    warning_code TEXT NOT NULL,
    stt_text TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '' CHECK (char_length(reason) <= 2000),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX fragment_manual_reviews_fragment_created_idx
    ON fragment_manual_reviews (fragment_id, created_at ASC, id ASC);

COMMIT;
