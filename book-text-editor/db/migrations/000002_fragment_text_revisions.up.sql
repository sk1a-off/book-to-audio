BEGIN;

CREATE TABLE fragment_text_revisions (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    fragment_id TEXT NOT NULL
        REFERENCES job_fragments(id)
        ON DELETE CASCADE,
    revision_number INTEGER NOT NULL CHECK (revision_number > 0),
    source TEXT NOT NULL CHECK (
        source IN ('original', 'manual', 'ai', 'restore')
    ),
    text TEXT NOT NULL CHECK (btrim(text) <> ''),
    parent_revision_id TEXT,
    model_id TEXT,
    prompt TEXT,
    reason TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (fragment_id, revision_number),
    UNIQUE (id, fragment_id),
    FOREIGN KEY (parent_revision_id, fragment_id)
        REFERENCES fragment_text_revisions(id, fragment_id)
        ON DELETE CASCADE,
    CHECK (
        (source = 'original' AND revision_number = 1 AND parent_revision_id IS NULL)
        OR
        (source <> 'original' AND revision_number > 1 AND parent_revision_id IS NOT NULL)
    ),
    CHECK (parent_revision_id IS NULL OR parent_revision_id <> id),
    CHECK (model_id IS NULL OR btrim(model_id) <> ''),
    CHECK (prompt IS NULL OR btrim(prompt) <> ''),
    CHECK (reason IS NULL OR btrim(reason) <> '')
);

CREATE INDEX fragment_text_revisions_parent_idx
    ON fragment_text_revisions (parent_revision_id, fragment_id)
    WHERE parent_revision_id IS NOT NULL;

INSERT INTO fragment_text_revisions (
    id,
    fragment_id,
    revision_number,
    source,
    text,
    created_at
)
SELECT
    job_fragments.id || ':original',
    job_fragments.id,
    1,
    'original',
    job_fragments.text,
    job_fragments.updated_at
FROM job_fragments;

COMMIT;
