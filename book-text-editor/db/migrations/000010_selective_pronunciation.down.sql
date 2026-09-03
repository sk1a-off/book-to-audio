BEGIN;

ALTER TABLE jobs
    DROP COLUMN pronunciation_rules,
    DROP COLUMN pronunciation_enabled;

COMMIT;
