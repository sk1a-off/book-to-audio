BEGIN;

ALTER TABLE jobs
    ADD COLUMN russian_text_version TEXT NOT NULL DEFAULT 'ru-selective-morph-v3',
    ADD COLUMN russian_selective_stress BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN russian_normalize_morphology BOOLEAN NOT NULL DEFAULT FALSE;

COMMIT;
