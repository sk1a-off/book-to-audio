BEGIN;

ALTER TABLE jobs
    DROP COLUMN IF EXISTS automatic_warning_retries,
    DROP COLUMN IF EXISTS whisper_word_timestamps,
    DROP COLUMN IF EXISTS whisper_vad_filter,
    DROP COLUMN IF EXISTS whisper_temperature,
    DROP COLUMN IF EXISTS whisper_patience,
    DROP COLUMN IF EXISTS whisper_beam_size,
    DROP COLUMN IF EXISTS omnivoice_fade_duration,
    DROP COLUMN IF EXISTS omnivoice_pad_duration,
    DROP COLUMN IF EXISTS omnivoice_audio_chunk_threshold,
    DROP COLUMN IF EXISTS omnivoice_audio_chunk_duration,
    DROP COLUMN IF EXISTS omnivoice_postprocess_output,
    DROP COLUMN IF EXISTS omnivoice_preprocess_prompt,
    DROP COLUMN IF EXISTS omnivoice_class_temperature,
    DROP COLUMN IF EXISTS omnivoice_position_temperature,
    DROP COLUMN IF EXISTS omnivoice_layer_penalty_factor,
    DROP COLUMN IF EXISTS omnivoice_t_shift,
    DROP COLUMN IF EXISTS omnivoice_denoise,
    DROP COLUMN IF EXISTS omnivoice_normalize_text,
    DROP COLUMN IF EXISTS omnivoice_speed,
    DROP COLUMN IF EXISTS omnivoice_guidance_scale,
    DROP COLUMN IF EXISTS omnivoice_num_steps;

COMMIT;
