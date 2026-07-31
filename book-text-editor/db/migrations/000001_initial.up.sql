BEGIN;

CREATE TABLE books (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    authors TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    format TEXT NOT NULL,
    chapters_count INTEGER NOT NULL CHECK (chapters_count >= 0),
    fragments_count INTEGER NOT NULL CHECK (fragments_count >= 0),
    status TEXT NOT NULL CHECK (status IN ('ready')),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE chapters (
    book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    number INTEGER NOT NULL CHECK (number > 0),
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    title TEXT NOT NULL,
    PRIMARY KEY (book_id, number),
    UNIQUE (book_id, ordinal)
);

CREATE TABLE book_fragments (
    book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    chapter_number INTEGER NOT NULL CHECK (chapter_number > 0),
    ordinal_in_chapter INTEGER NOT NULL CHECK (ordinal_in_chapter > 0),
    text TEXT NOT NULL CHECK (text <> ''),
    PRIMARY KEY (book_id, ordinal),
    UNIQUE (book_id, chapter_number, ordinal_in_chapter),
    FOREIGN KEY (book_id, chapter_number)
        REFERENCES chapters(book_id, number)
        ON DELETE CASCADE
);

CREATE TABLE voices (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    mode TEXT NOT NULL CHECK (mode IN ('voice_clone')),
    format TEXT NOT NULL,
    content_type TEXT NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    has_reference_text BOOLEAN NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ready')),
    reference_text TEXT NOT NULL,
    reference_audio BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (size_bytes = octet_length(reference_audio)),
    CHECK (has_reference_text = (reference_text <> ''))
);

CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    book_id TEXT NOT NULL REFERENCES books(id) ON DELETE RESTRICT,
    voice_id TEXT NOT NULL REFERENCES voices(id) ON DELETE RESTRICT,
    status TEXT NOT NULL CHECK (
        status IN (
            'queued',
            'running',
            'completed',
            'completed_with_warnings',
            'failed'
        )
    ),
    fragments_count INTEGER NOT NULL CHECK (fragments_count >= 0),
    fragments_pending INTEGER NOT NULL CHECK (fragments_pending >= 0),
    fragments_ready INTEGER NOT NULL CHECK (fragments_ready >= 0),
    fragments_warnings INTEGER NOT NULL CHECK (fragments_warnings >= 0),
    fragments_failed INTEGER NOT NULL CHECK (fragments_failed >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (id, book_id),
    CHECK (
        fragments_count =
        fragments_pending +
        fragments_ready +
        fragments_warnings +
        fragments_failed
    )
);

CREATE INDEX jobs_book_latest_idx
    ON jobs (book_id, created_at DESC, id DESC);

CREATE INDEX jobs_created_idx
    ON jobs (created_at DESC, id DESC);

CREATE INDEX jobs_status_created_idx
    ON jobs (status, created_at DESC, id DESC);

CREATE TABLE job_fragments (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL,
    book_id TEXT NOT NULL,
    source_ordinal INTEGER NOT NULL CHECK (source_ordinal > 0),
    chapter_number INTEGER NOT NULL CHECK (chapter_number > 0),
    chapter_title TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    text TEXT NOT NULL CHECK (text <> ''),
    stt_text TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (
        status IN ('pending', 'generating', 'ready', 'warning', 'failed')
    ),
    warning_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    attempt INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    audio_pcm BYTEA NOT NULL DEFAULT ''::BYTEA,
    sample_rate INTEGER NOT NULL DEFAULT 0 CHECK (sample_rate >= 0),
    channels INTEGER NOT NULL DEFAULT 0 CHECK (channels >= 0),
    sample_width INTEGER NOT NULL DEFAULT 0 CHECK (sample_width >= 0),
    duration_ms INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    stt_language TEXT NOT NULL DEFAULT '',
    worker_notes TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (job_id, ordinal),
    FOREIGN KEY (job_id, book_id)
        REFERENCES jobs(id, book_id)
        ON DELETE CASCADE,
    FOREIGN KEY (book_id, source_ordinal)
        REFERENCES book_fragments(book_id, ordinal)
        ON DELETE RESTRICT
);

CREATE INDEX job_fragments_issues_idx
    ON job_fragments (job_id, status, ordinal);

CREATE INDEX job_fragments_chapter_archive_idx
    ON job_fragments (job_id, chapter_number, ordinal);

CREATE TABLE fragment_attempts (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    fragment_id TEXT NOT NULL REFERENCES job_fragments(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL CHECK (attempt >= 0),
    text TEXT NOT NULL,
    stt_text TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (
        status IN ('pending', 'generating', 'ready', 'warning', 'failed')
    ),
    warning_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    audio_pcm BYTEA NOT NULL DEFAULT ''::BYTEA,
    sample_rate INTEGER NOT NULL DEFAULT 0 CHECK (sample_rate >= 0),
    channels INTEGER NOT NULL DEFAULT 0 CHECK (channels >= 0),
    sample_width INTEGER NOT NULL DEFAULT 0 CHECK (sample_width >= 0),
    duration_ms INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    stt_language TEXT NOT NULL DEFAULT '',
    worker_notes TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
    completed_at TIMESTAMPTZ NOT NULL,
    UNIQUE (fragment_id, attempt)
);

COMMIT;
