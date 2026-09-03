BEGIN;

ALTER TABLE jobs
    ADD COLUMN pronunciation_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN pronunciation_rules TEXT NOT NULL DEFAULT ''
        CHECK (octet_length(pronunciation_rules) <= 65536);

COMMIT;
