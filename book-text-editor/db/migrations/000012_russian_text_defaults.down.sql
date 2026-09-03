BEGIN;

ALTER TABLE jobs
    ALTER COLUMN russian_selective_stress SET DEFAULT FALSE,
    ALTER COLUMN russian_normalize_morphology SET DEFAULT FALSE;

COMMIT;
