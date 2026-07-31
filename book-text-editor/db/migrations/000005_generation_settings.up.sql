BEGIN;

ALTER TABLE jobs
    ADD COLUMN omnivoice_num_steps INTEGER NOT NULL DEFAULT 32
        CHECK (omnivoice_num_steps BETWEEN 1 AND 100),
    ADD COLUMN omnivoice_guidance_scale DOUBLE PRECISION NOT NULL DEFAULT 2.0
        CHECK (
            omnivoice_guidance_scale >= 0.0
            AND omnivoice_guidance_scale <= 10.0
        ),
    ADD COLUMN omnivoice_speed DOUBLE PRECISION NOT NULL DEFAULT 1.0
        CHECK (omnivoice_speed >= 0.5 AND omnivoice_speed <= 2.0),
    ADD COLUMN omnivoice_normalize_text BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN omnivoice_denoise BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN omnivoice_t_shift DOUBLE PRECISION NOT NULL DEFAULT 0.1
        CHECK (omnivoice_t_shift BETWEEN 0.001 AND 10.0),
    ADD COLUMN omnivoice_layer_penalty_factor DOUBLE PRECISION
        NOT NULL DEFAULT 5.0
        CHECK (omnivoice_layer_penalty_factor BETWEEN 0.0 AND 20.0),
    ADD COLUMN omnivoice_position_temperature DOUBLE PRECISION
        NOT NULL DEFAULT 5.0
        CHECK (omnivoice_position_temperature BETWEEN 0.0 AND 20.0),
    ADD COLUMN omnivoice_class_temperature DOUBLE PRECISION
        NOT NULL DEFAULT 0.0
        CHECK (omnivoice_class_temperature BETWEEN 0.0 AND 10.0),
    ADD COLUMN omnivoice_preprocess_prompt BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN omnivoice_postprocess_output BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN omnivoice_audio_chunk_duration DOUBLE PRECISION
        NOT NULL DEFAULT 15.0
        CHECK (omnivoice_audio_chunk_duration BETWEEN 1.0 AND 120.0),
    ADD COLUMN omnivoice_audio_chunk_threshold DOUBLE PRECISION
        NOT NULL DEFAULT 30.0
        CHECK (omnivoice_audio_chunk_threshold BETWEEN 1.0 AND 600.0),
    ADD COLUMN omnivoice_pad_duration DOUBLE PRECISION NOT NULL DEFAULT 0.1
        CHECK (omnivoice_pad_duration BETWEEN 0.0 AND 5.0),
    ADD COLUMN omnivoice_fade_duration DOUBLE PRECISION NOT NULL DEFAULT 0.1
        CHECK (omnivoice_fade_duration BETWEEN 0.0 AND 5.0),
    ADD COLUMN whisper_beam_size INTEGER NOT NULL DEFAULT 5
        CHECK (whisper_beam_size BETWEEN 1 AND 10),
    ADD COLUMN whisper_patience DOUBLE PRECISION NOT NULL DEFAULT 1.0
        CHECK (whisper_patience >= 0.1 AND whisper_patience <= 2.0),
    ADD COLUMN whisper_temperature DOUBLE PRECISION NOT NULL DEFAULT 0.0
        CHECK (
            whisper_temperature >= 0.0
            AND whisper_temperature <= 1.0
        ),
    ADD COLUMN whisper_vad_filter BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN whisper_word_timestamps BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN automatic_warning_retries INTEGER NOT NULL DEFAULT 5
        CHECK (automatic_warning_retries BETWEEN 0 AND 10);

COMMIT;
