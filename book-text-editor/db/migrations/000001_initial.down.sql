BEGIN;

DROP TABLE IF EXISTS rewrite_task_fragments;
DROP TABLE IF EXISTS rewrite_tasks;
DROP TABLE IF EXISTS fragment_manual_reviews;
DROP TABLE IF EXISTS fragment_text_revisions;
DROP TABLE IF EXISTS fragment_attempts;
DROP TABLE IF EXISTS job_fragments;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS voices;
DROP TABLE IF EXISTS book_fragments;
DROP TABLE IF EXISTS chapters;
DROP TABLE IF EXISTS books;

COMMIT;
