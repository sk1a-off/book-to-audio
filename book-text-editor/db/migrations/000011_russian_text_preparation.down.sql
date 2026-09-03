BEGIN;

ALTER TABLE jobs
    DROP COLUMN russian_normalize_morphology,
    DROP COLUMN russian_selective_stress,
    DROP COLUMN russian_text_version;

COMMIT;
