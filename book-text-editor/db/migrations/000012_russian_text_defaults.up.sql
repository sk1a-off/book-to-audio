BEGIN;

ALTER TABLE jobs
    ALTER COLUMN russian_selective_stress SET DEFAULT TRUE,
    ALTER COLUMN russian_normalize_morphology SET DEFAULT TRUE;

COMMIT;
